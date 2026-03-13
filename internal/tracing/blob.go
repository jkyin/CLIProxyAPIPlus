package tracing

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

type BlobConfig struct {
	BaseDir         string
	InlineMaxBytes  int64
	MaxBodyBytes    int64
	PreferFile      bool
	ContentType     string
	ContentEncoding string
}

type BlobCollector struct {
	cfg       BlobConfig
	inline    bytes.Buffer
	file      *os.File
	fileRel   string
	size      int64
	truncated bool
}

func NewBlobCollector(cfg BlobConfig) (*BlobCollector, error) {
	if cfg.InlineMaxBytes <= 0 {
		cfg.InlineMaxBytes = 64 * 1024
	}
	return &BlobCollector{cfg: cfg}, nil
}

func (c *BlobCollector) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if c.cfg.MaxBodyBytes > 0 && c.size >= c.cfg.MaxBodyBytes {
		c.truncated = true
		return len(p), nil
	}
	toWrite := p
	if c.cfg.MaxBodyBytes > 0 {
		remain := c.cfg.MaxBodyBytes - c.size
		if remain < int64(len(p)) {
			toWrite = p[:remain]
			c.truncated = true
		}
	}
	if len(toWrite) == 0 {
		return len(p), nil
	}
	if c.file == nil && (c.cfg.PreferFile || c.size+int64(len(toWrite)) > c.cfg.InlineMaxBytes) {
		if err := c.ensureFile(); err != nil {
			return 0, err
		}
		if c.inline.Len() > 0 {
			if _, err := c.file.Write(c.inline.Bytes()); err != nil {
				return 0, err
			}
			c.inline.Reset()
		}
	}
	if c.file != nil {
		if _, err := c.file.Write(toWrite); err != nil {
			return 0, err
		}
	} else if _, err := c.inline.Write(toWrite); err != nil {
		return 0, err
	}
	c.size += int64(len(toWrite))
	return len(p), nil
}

func (c *BlobCollector) Close(complete bool) (*BlobRecord, error) {
	if c.file != nil {
		if err := c.file.Close(); err != nil {
			return nil, err
		}
		c.file = nil
	}
	record := &BlobRecord{
		BlobID:          MustNewID(),
		StorageKind:     "inline",
		SizeBytes:       c.size,
		ContentType:     c.cfg.ContentType,
		ContentEncoding: c.cfg.ContentEncoding,
		Complete:        complete,
		Truncated:       c.truncated,
	}
	if c.fileRel != "" {
		record.StorageKind = "file"
		record.FileRelPath = c.fileRel
		data, err := os.ReadFile(filepath.Join(c.cfg.BaseDir, c.fileRel))
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		record.SHA256 = hex.EncodeToString(sum[:])
		return record, nil
	}
	record.InlineBytes = cloneBytes(c.inline.Bytes())
	sum := sha256.Sum256(record.InlineBytes)
	record.SHA256 = hex.EncodeToString(sum[:])
	return record, nil
}

func (c *BlobCollector) ensureFile() error {
	if c.file != nil {
		return nil
	}
	if c.cfg.BaseDir == "" {
		return fmt.Errorf("tracing blob collector: base dir is empty")
	}
	blobID := MustNewID()
	relPath := filepath.Join("bodies", blobID[:2], blobID+".bin")
	fullPath := filepath.Join(c.cfg.BaseDir, relPath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(fullPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	c.file = file
	c.fileRel = relPath
	return nil
}

func CollectBlobBytes(cfg BlobConfig, body []byte, complete bool) (*BlobRecord, error) {
	if len(body) == 0 {
		return nil, nil
	}
	collector, err := NewBlobCollector(cfg)
	if err != nil {
		return nil, err
	}
	if _, err := collector.Write(body); err != nil {
		return nil, err
	}
	return collector.Close(complete)
}
