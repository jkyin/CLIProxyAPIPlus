package management

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

func (h *Handler) GetTracingStatus(c *gin.Context) {
	if h == nil || h.tracingRecorder == nil || !h.tracingRecorder.Enabled() {
		c.JSON(http.StatusOK, gin.H{"enabled": false})
		return
	}
	status, err := h.tracingRecorder.Status(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, status)
}

func (h *Handler) GetTracingEvents(c *gin.Context) {
	if h == nil || h.tracingRecorder == nil || !h.tracingRecorder.Enabled() {
		c.JSON(http.StatusOK, gin.H{"events": []any{}})
		return
	}
	afterSeq, _ := strconv.ParseInt(strings.TrimSpace(c.Query("after_seq")), 10, 64)
	limit, _ := strconv.Atoi(strings.TrimSpace(c.Query("limit")))
	events, err := h.tracingRecorder.ListEvents(c.Request.Context(), afterSeq, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"events":     events,
		"latest_seq": h.tracingRecorder.LatestSeq(),
	})
}

func (h *Handler) GetTracingRequest(c *gin.Context) {
	if h == nil || h.tracingRecorder == nil || !h.tracingRecorder.Enabled() {
		c.JSON(http.StatusNotFound, gin.H{"error": "tracing disabled"})
		return
	}
	requestID := strings.TrimSpace(c.Param("request_id"))
	record, err := h.tracingRecorder.GetRequest(c.Request.Context(), requestID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if record == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "request not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"request": record})
}

func (h *Handler) GetTracingAttempts(c *gin.Context) {
	if h == nil || h.tracingRecorder == nil || !h.tracingRecorder.Enabled() {
		c.JSON(http.StatusNotFound, gin.H{"error": "tracing disabled"})
		return
	}
	requestID := strings.TrimSpace(c.Param("request_id"))
	attempts, err := h.tracingRecorder.ListAttempts(c.Request.Context(), requestID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"attempts": attempts})
}

func (h *Handler) GetTracingUsage(c *gin.Context) {
	if h == nil || h.tracingRecorder == nil || !h.tracingRecorder.Enabled() {
		c.JSON(http.StatusNotFound, gin.H{"error": "tracing disabled"})
		return
	}
	requestID := strings.TrimSpace(c.Param("request_id"))
	usage, err := h.tracingRecorder.GetUsage(c.Request.Context(), requestID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if usage == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "usage not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"usage": usage})
}

func (h *Handler) GetTracingBlob(c *gin.Context) {
	if h == nil || h.tracingRecorder == nil || !h.tracingRecorder.Enabled() {
		c.JSON(http.StatusNotFound, gin.H{"error": "tracing disabled"})
		return
	}
	blobID := strings.TrimSpace(c.Param("blob_id"))
	record, err := h.tracingRecorder.GetBlob(c.Request.Context(), blobID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if record == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "blob not found"})
		return
	}
	if c.Query("raw") == "1" {
		c.Data(http.StatusOK, record.ContentType, record.InlineBytes)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"blob": record,
		"data": string(record.InlineBytes),
	})
}

func (h *Handler) TracingSSE(c *gin.Context) {
	if h == nil || h.tracingRecorder == nil || !h.tracingRecorder.Enabled() {
		c.JSON(http.StatusNotFound, gin.H{"error": "tracing disabled"})
		return
	}
	ch, cancel := h.tracingRecorder.Subscribe()
	defer cancel()

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Status(http.StatusOK)
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "streaming unsupported"})
		return
	}

	latest := h.tracingRecorder.LatestSeq()
	if latest > 0 {
		_, _ = c.Writer.Write([]byte(fmt.Sprintf("event: ready\ndata: {\"latest_seq\":%d}\n\n", latest)))
		flusher.Flush()
	}
	for {
		select {
		case <-c.Request.Context().Done():
			return
		case seq, ok := <-ch:
			if !ok {
				return
			}
			payload, _ := json.Marshal(gin.H{
				"latest_seq": seq,
				"ts":         time.Now().UTC().Format(time.RFC3339Nano),
			})
			_, _ = c.Writer.Write([]byte("event: seq\n"))
			_, _ = c.Writer.Write([]byte("data: "))
			_, _ = c.Writer.Write(payload)
			_, _ = c.Writer.Write([]byte("\n\n"))
			flusher.Flush()
		}
	}
}
