package middleware

import (
	"bytes"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/tracing"
	"github.com/tidwall/gjson"
)

var tracingPathPrefixes = []string{
	"/v1/models",
	"/v1/chat/completions",
	"/v1/completions",
	"/v1/messages",
	"/v1/responses",
	"/v1beta/models/",
	"/api/provider/",
}

func TracingMiddleware(cfg *config.Config, recorder tracing.Recorder) gin.HandlerFunc {
	return func(c *gin.Context) {
		if recorder == nil || !recorder.Enabled() || !shouldTracePath(c.Request) {
			c.Next()
			return
		}

		startedAt := time.Now().UTC()
		requestID := tracing.MustNewID()
		legacyRequestID := logging.GetGinRequestID(c)
		body, err := readAndRestoreBody(c)
		if err != nil {
			c.Next()
			return
		}

		var requestBodyBlobID string
		blobCfg := tracing.BlobConfig{
			BaseDir:         filepath.Dir(recorder.DBPath()),
			InlineMaxBytes:  cfg.Tracing.BodyInlineMaxBytes,
			MaxBodyBytes:    cfg.Tracing.MaxBodyBytes,
			PreferFile:      false,
			ContentType:     strings.TrimSpace(c.GetHeader("Content-Type")),
			ContentEncoding: strings.TrimSpace(c.GetHeader("Content-Encoding")),
		}
		if blob, errBlob := tracing.CollectBlobBytes(blobCfg, body, true); errBlob == nil && blob != nil {
			_ = recorder.SaveBlob(c.Request.Context(), blob)
			requestBodyBlobID = blob.BlobID
		}

		isStream := isTracingStreamRequest(c.Request, body)
		start := tracing.RequestStart{
			RequestID:           requestID,
			LegacyRequestID:     legacyRequestID,
			StartedAt:           startedAt,
			Method:              c.Request.Method,
			Scheme:              requestScheme(c.Request),
			Host:                c.Request.Host,
			Path:                c.Request.URL.Path,
			Query:               c.Request.URL.RawQuery,
			RequestHeaders:      c.Request.Header.Clone(),
			RequestBodyBlobID:   requestBodyBlobID,
			IsStream:            isStream,
			RequestedModel:      gjson.GetBytes(body, "model").String(),
			ClientCorrelationID: strings.TrimSpace(c.GetHeader("Idempotency-Key")),
		}
		state := tracing.RecordRequestStart(c.Request.Context(), start)
		if state != nil {
			state.SetBlobConfig(blobCfg)
			tracing.AttachRequestState(c, state)
		}
		c.Next()
	}
}

func readAndRestoreBody(c *gin.Context) ([]byte, error) {
	if c == nil || c.Request == nil || c.Request.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return nil, err
	}
	c.Request.Body = io.NopCloser(bytes.NewBuffer(body))
	return body, nil
}

func shouldTracePath(req *http.Request) bool {
	if req == nil || req.URL == nil {
		return false
	}
	path := req.URL.Path
	if strings.HasPrefix(path, "/v0/management") || strings.HasPrefix(path, "/management") {
		return false
	}
	for _, prefix := range tracingPathPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func requestScheme(req *http.Request) string {
	if req == nil {
		return "http"
	}
	if req.TLS != nil {
		return "https"
	}
	return "http"
}

func isTracingStreamRequest(req *http.Request, body []byte) bool {
	if req != nil && req.Method == http.MethodGet && strings.EqualFold(strings.TrimSpace(req.Header.Get("Upgrade")), "websocket") {
		return true
	}
	return bytes.Contains(body, []byte(`"stream":true`)) || bytes.Contains(body, []byte(`"stream": true`))
}
