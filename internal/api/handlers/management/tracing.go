package management

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/tracing"
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

func (h *Handler) GetTracingRequestSummary(c *gin.Context) {
	if h == nil || h.tracingRecorder == nil || !h.tracingRecorder.Enabled() {
		c.JSON(http.StatusNotFound, gin.H{"error": "tracing disabled"})
		return
	}
	requestID := strings.TrimSpace(c.Param("request_id"))
	record, err := h.tracingRecorder.GetRequestSummary(c.Request.Context(), requestID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if record == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "summary not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"summary": record})
}

func (h *Handler) GetTracingRequests(c *gin.Context) {
	if h == nil || h.tracingRecorder == nil || !h.tracingRecorder.Enabled() {
		c.JSON(http.StatusOK, tracing.RequestSummaryPage{Items: []tracing.RequestSummaryRecord{}})
		return
	}

	filter := tracing.RequestSummaryFilter{
		Limit:          parseIntQuery(c, "limit", 50),
		Offset:         parseIntQuery(c, "offset", 0),
		Search:         strings.TrimSpace(c.Query("search")),
		Status:         strings.TrimSpace(c.Query("status")),
		Provider:       strings.TrimSpace(c.Query("provider")),
		RequestedModel: strings.TrimSpace(c.Query("requested_model")),
		HasUsageOnly:   parseBoolQuery(c, "has_usage_only"),
		StreamOnly:     parseOptionalBoolQuery(c, "stream_only"),
	}

	page, err := h.tracingRecorder.ListRequestSummaries(c.Request.Context(), filter)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if page.Items == nil {
		page.Items = []tracing.RequestSummaryRecord{}
	}
	c.JSON(http.StatusOK, page)
}

func (h *Handler) GetTracingRequestDetail(c *gin.Context) {
	if h == nil || h.tracingRecorder == nil || !h.tracingRecorder.Enabled() {
		c.JSON(http.StatusNotFound, gin.H{"error": "tracing disabled"})
		return
	}
	requestID := strings.TrimSpace(c.Param("request_id"))
	record, err := h.tracingRecorder.GetRequestDetail(c.Request.Context(), requestID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if record == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "request not found"})
		return
	}
	c.JSON(http.StatusOK, record)
}

func (h *Handler) DeleteTracingRequest(c *gin.Context) {
	if h == nil || h.tracingRecorder == nil || !h.tracingRecorder.Enabled() {
		c.JSON(http.StatusNotFound, gin.H{"error": "tracing disabled"})
		return
	}
	requestID := strings.TrimSpace(c.Param("request_id"))
	deleted, err := h.tracingRecorder.DeleteRequest(c.Request.Context(), requestID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !deleted {
		c.JSON(http.StatusNotFound, gin.H{"error": "request not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": true, "request_id": requestID})
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

func (h *Handler) TracingRequestSummariesStream(c *gin.Context) {
	if h == nil || h.tracingRecorder == nil || !h.tracingRecorder.Enabled() {
		c.JSON(http.StatusNotFound, gin.H{"error": "tracing disabled"})
		return
	}
	ch, cancel := h.tracingRecorder.SubscribeRequestSummaries()
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

	readyPayload, _ := json.Marshal(tracing.RequestSummaryEvent{
		Type:      tracing.RequestSummaryEventReady,
		LatestSeq: h.tracingRecorder.LatestSeq(),
		TS:        time.Now().UTC().Format(time.RFC3339Nano),
	})
	_, _ = c.Writer.Write([]byte("event: " + tracing.RequestSummaryEventReady + "\n"))
	_, _ = c.Writer.Write([]byte("data: "))
	_, _ = c.Writer.Write(readyPayload)
	_, _ = c.Writer.Write([]byte("\n\n"))
	flusher.Flush()

	for {
		select {
		case <-c.Request.Context().Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			payload, _ := json.Marshal(event)
			eventName := strings.TrimSpace(event.Type)
			if eventName == "" {
				eventName = tracing.RequestSummaryEventUpdated
			}
			_, _ = c.Writer.Write([]byte("event: " + eventName + "\n"))
			_, _ = c.Writer.Write([]byte("data: "))
			_, _ = c.Writer.Write(payload)
			_, _ = c.Writer.Write([]byte("\n\n"))
			flusher.Flush()
		}
	}
}

func parseIntQuery(c *gin.Context, key string, defaultValue int) int {
	value, err := strconv.Atoi(strings.TrimSpace(c.Query(key)))
	if err != nil {
		return defaultValue
	}
	return value
}

func parseBoolQuery(c *gin.Context, key string) bool {
	value, _ := strconv.ParseBool(strings.TrimSpace(c.Query(key)))
	return value
}

func parseOptionalBoolQuery(c *gin.Context, key string) *bool {
	rawValue := strings.TrimSpace(c.Query(key))
	if rawValue == "" {
		return nil
	}
	value, err := strconv.ParseBool(rawValue)
	if err != nil {
		return nil
	}
	return &value
}
