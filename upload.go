package profiler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ingestPath is the endpoint profiles are posted to.
const ingestPath = "/api/v1/ingest/profiles"

// ErrQueueFull reports that an upload was dropped because the queue was full.
var ErrQueueFull = errors.New("profiler: upload queue is full, profile dropped")

// upload is one queued profile.
type upload struct {
	metadata Metadata
	payload  []byte
}

// uploader ships profiles to the platform from a single background goroutine.
type uploader struct {
	endpoint string
	apiKey   string
	client   *http.Client
	timeout  time.Duration
	logger   Logger

	queue     chan upload
	closeOnce sync.Once
}

func newUploader(cfg Config, logger Logger) *uploader {
	return &uploader{
		endpoint: strings.TrimRight(strings.TrimSpace(cfg.Endpoint), "/") + ingestPath,
		apiKey:   strings.TrimSpace(cfg.APIKey),
		client:   cfg.HTTPClient,
		timeout:  cfg.UploadTimeout,
		logger:   logger,
		queue:    make(chan upload, cfg.QueueSize),
	}
}

// start launches the worker goroutine.
func (u *uploader) start(wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		for job := range u.queue {
			u.send(job)
		}
	}()
}

// enqueue queues a profile without ever blocking the caller.
//
// A full queue drops the profile and says so. The alternative — blocking the
// application until the profiling backend recovers — is exactly the failure
// mode this SDK exists to avoid.
func (u *uploader) enqueue(job upload) error {
	select {
	case u.queue <- job:
		return nil
	default:
		u.logger.Errorf("upload queue full, dropping %s profile", job.metadata.ProfileType)
		return ErrQueueFull
	}
}

func (u *uploader) close() {
	u.closeOnce.Do(func() { close(u.queue) })
}

// send performs one upload attempt.
//
// There is no retry. A dropped profile costs a data point; a retry storm
// against a struggling backend costs an incident, and the next capture will be
// along shortly anyway.
func (u *uploader) send(job upload) {
	body, contentType, err := buildMultipart(job)
	if err != nil {
		u.logger.Errorf("could not build the upload body: %v", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), u.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.endpoint, bytes.NewReader(body))
	if err != nil {
		u.logger.Errorf("could not build the upload request: %v", err)
		return
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+u.apiKey)
	req.Header.Set("User-Agent", userAgent)

	resp, err := u.client.Do(req)
	if err != nil {
		u.logger.Errorf("upload of the %s profile failed: %v", job.metadata.ProfileType, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= http.StatusBadRequest {
		// Read a little of the body: the platform's structured error explains
		// what is wrong far better than the status code alone.
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		u.logger.Errorf("upload of the %s profile rejected with %s: %s",
			job.metadata.ProfileType, resp.Status, strings.TrimSpace(string(detail)))
		return
	}

	// Drain what is left so the connection can be reused.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	u.logger.Debugf("uploaded %s profile (%d bytes)", job.metadata.ProfileType, len(job.payload))
}

// userAgent identifies the SDK in the platform's request logs.
const userAgent = "pecgoprof-go-sdk/0.1"

// buildMultipart assembles the metadata and profile parts.
func buildMultipart(job upload) (body []byte, contentType string, err error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	metadataPart, err := writer.CreateFormField("metadata")
	if err != nil {
		return nil, "", fmt.Errorf("create the metadata part: %w", err)
	}
	if err := json.NewEncoder(metadataPart).Encode(job.metadata); err != nil {
		return nil, "", fmt.Errorf("encode the metadata: %w", err)
	}

	// The filename is cosmetic — the server generates its own storage key and
	// ignores whatever is sent here.
	profilePart, err := writer.CreateFormFile("profile", job.metadata.ProfileType+".pprof")
	if err != nil {
		return nil, "", fmt.Errorf("create the profile part: %w", err)
	}
	if _, err := profilePart.Write(job.payload); err != nil {
		return nil, "", fmt.Errorf("write the profile: %w", err)
	}

	if err := writer.Close(); err != nil {
		return nil, "", fmt.Errorf("close the multipart writer: %w", err)
	}
	return buf.Bytes(), writer.FormDataContentType(), nil
}
