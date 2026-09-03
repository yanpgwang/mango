// Package mango provides a standalone client for Mango's development HTTP API.
package mango

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Config controls HTTP transport. Agent execution helpers are configured
// separately and never change the Client's transport settings.
type Config struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
	// RequestTimeout applies to finite requests, not SSE or binary downloads.
	// Zero uses HTTPClient.Timeout if set, otherwise 60 seconds. Negative disables it.
	RequestTimeout time.Duration
}

type Client struct {
	baseURL        string
	apiKey         string
	http           *http.Client
	requestTimeout time.Duration
}

// New creates a client. Redirects are never followed, preventing credentials
// from being forwarded to a redirect target. No request retries are added.
func New(config Config) (*Client, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("mango: BaseURL must be an absolute HTTP or HTTPS URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("mango: BaseURL cannot contain credentials, query, or fragment")
	}
	client := http.Client{}
	if config.HTTPClient != nil {
		client = *config.HTTPClient
	}
	timeout := config.RequestTimeout
	if timeout == 0 {
		timeout = client.Timeout
	}
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	client.Timeout = 0
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{baseURL: endpoint, apiKey: config.APIKey, http: &client, requestTimeout: timeout}, nil
}

// APIError is a non-2xx response. Body is capped at 1 MiB; do not log it when
// requests could contain sensitive data. RequestID is safe to use for correlation.
type APIError struct {
	StatusCode int
	Type       string
	Message    string
	RequestID  string
	Header     http.Header
	Body       []byte
}

func (e *APIError) Error() string {
	return fmt.Sprintf("mango: HTTP %d %s: %s (request %s)", e.StatusCode, e.Type, e.Message, e.RequestID)
}

// Upload is streamed into a multipart request. Caller owns Reader and must keep
// it available until the request finishes. Filename may include Skill paths.
type Upload struct {
	Filename    string
	ContentType string
	Reader      io.Reader
}

// Download owns a streaming response body. Always Close it, including when
// abandoning a partial download. The request context controls its lifetime.
type Download struct {
	io.ReadCloser
	Header     http.Header
	StatusCode int
}

func escapePath(value string) string {
	// PathEscape leaves dot-only segments untouched, which proxies can normalize.
	if value == "." {
		return "%2E"
	}
	if value == ".." {
		return "%2E%2E"
	}
	return url.PathEscape(value)
}

func addQuery[T any](query url.Values, name string, value Optional[T]) {
	if v, ok := value.Get(); ok {
		query.Add(name, fmt.Sprint(v))
	}
}

func addQueryArray[T any](query url.Values, name string, value Optional[[]T]) {
	if values, ok := value.Get(); ok {
		for _, value := range values {
			query.Add(name, fmt.Sprint(value))
		}
	}
}

func (c *Client) request(ctx context.Context, method, path string, query url.Values, body io.Reader, contentType, accept string, auth bool) (*http.Response, error) {
	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("mango: create request: %w", err)
	}
	if auth && c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "mango-go/0.1.0")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	response, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mango: request: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		var envelope struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
			RequestID string `json:"request_id"`
		}
		_ = json.Unmarshal(data, &envelope)
		requestID := response.Header.Get("Request-Id")
		if requestID == "" {
			requestID = response.Header.Get("X-Request-Id")
		}
		if requestID == "" {
			requestID = envelope.RequestID
		}
		message := envelope.Error.Message
		if message == "" {
			message = http.StatusText(response.StatusCode)
		}
		return nil, &APIError{StatusCode: response.StatusCode, Type: envelope.Error.Type, Message: message, RequestID: requestID, Header: response.Header.Clone(), Body: data}
	}
	return response, nil
}

func (c *Client) finiteContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.requestTimeout > 0 {
		return context.WithTimeout(ctx, c.requestTimeout)
	}
	return context.WithCancel(ctx)
}

func (c *Client) doJSON(ctx context.Context, method, path string, query url.Values, body any, output any, auth bool) error {
	ctx, cancel := c.finiteContext(ctx)
	defer cancel()
	var input io.Reader
	contentType := ""
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("mango: encode request: %w", err)
		}
		input, contentType = bytes.NewReader(data), "application/json"
	}
	response, err := c.request(ctx, method, path, query, input, contentType, "application/json", auth)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if output == nil {
		_, err = io.Copy(io.Discard, response.Body)
		return err
	}
	if err := json.NewDecoder(response.Body).Decode(output); err != nil {
		return fmt.Errorf("mango: decode response: %w", err)
	}
	return nil
}

func (c *Client) download(ctx context.Context, method, path string, query url.Values, accept string, auth bool) (*Download, error) {
	response, err := c.request(ctx, method, path, query, nil, "", accept, auth)
	if err != nil {
		return nil, err
	}
	return &Download{ReadCloser: response.Body, Header: response.Header.Clone(), StatusCode: response.StatusCode}, nil
}

type multipartPart struct {
	name   string
	upload *Upload
	value  string
}

func (c *Client) doMultipart(ctx context.Context, method, path string, query url.Values, parts []multipartPart, output any, auth bool) error {
	ctx, cancel := c.finiteContext(ctx)
	defer cancel()
	reader, writer := io.Pipe()
	defer reader.Close()
	multipartWriter := multipart.NewWriter(writer)
	go func() {
		_ = writer.CloseWithError(writeMultipart(multipartWriter, parts))
	}()
	response, err := c.request(ctx, method, path, query, reader, multipartWriter.FormDataContentType(), "application/json", auth)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if err := json.NewDecoder(response.Body).Decode(output); err != nil {
		return fmt.Errorf("mango: decode response: %w", err)
	}
	return nil
}

func writeMultipart(writer *multipart.Writer, parts []multipartPart) error {
	for _, part := range parts {
		if part.upload == nil {
			if err := writer.WriteField(part.name, part.value); err != nil {
				return err
			}
			continue
		}
		if part.upload.Reader == nil {
			return errors.New("mango: upload Reader is required")
		}
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", "form-data; name="+quoteMultipart(part.name)+"; filename="+quoteMultipart(part.upload.Filename))
		contentType := part.upload.ContentType
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		if strings.ContainsAny(contentType, "\r\n") {
			return errors.New("mango: invalid upload ContentType")
		}
		header.Set("Content-Type", contentType)
		destination, err := writer.CreatePart(header)
		if err != nil {
			return err
		}
		if _, err := io.Copy(destination, part.upload.Reader); err != nil {
			return err
		}
	}
	return writer.Close()
}

func quoteMultipart(value string) string {
	value = strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\r", "%0D", "\n", "%0A").Replace(value)
	return "\"" + value + "\""
}

// ParseRetryMilliseconds parses an SSE retry field without enabling retries.
func ParseRetryMilliseconds(value string) (time.Duration, bool) {
	if value == "" || strings.Trim(value, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n > int64((1<<63-1)/time.Millisecond) {
		return 0, false
	}
	return time.Duration(n) * time.Millisecond, true
}
