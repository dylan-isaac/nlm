// Package grpcendpoint handles gRPC-style endpoints for NotebookLM
package grpcendpoint

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// Option configures a Client
type Option func(*Client)

// WithHTTPClient sets the HTTP client
func WithHTTPClient(client *http.Client) Option {
	return func(c *Client) {
		c.httpClient = client
	}
}

// WithDebug enables debug output
func WithDebug(debug bool) Option {
	return func(c *Client) {
		c.debug = debug
	}
}

// WithHeaders adds additional headers to outgoing requests
func WithHeaders(headers map[string]string) Option {
	return func(c *Client) {
		for k, v := range headers {
			c.headers[k] = v
		}
	}
}

// WithURLParams adds additional URL query parameters
func WithURLParams(params map[string]string) Option {
	return func(c *Client) {
		for k, v := range params {
			c.urlParams[k] = v
		}
	}
}

// Client handles gRPC-style endpoint requests
type Client struct {
	authToken  string
	cookies    string
	httpClient *http.Client
	debug      bool
	headers    map[string]string
	urlParams  map[string]string
	reqid      *reqIDGenerator
}

// NewClient creates a new gRPC endpoint client
func NewClient(authToken, cookies string, opts ...Option) *Client {
	c := &Client{
		authToken:  authToken,
		cookies:    cookies,
		httpClient: &http.Client{},
		headers:    make(map[string]string),
		urlParams:  make(map[string]string),
		reqid:      newReqIDGenerator(),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Request represents a gRPC-style request
type Request struct {
	Endpoint string      // e.g., "/google.internal.labs.tailwind.orchestration.v1.LabsTailwindOrchestrationService/GenerateFreeFormStreamed"
	Body     interface{} // The request body (will be JSON encoded into f.req)
}

// buildRequest constructs the HTTP request for a gRPC endpoint call.
func (c *Client) buildRequest(req Request) (*http.Request, string, error) {
	baseURL := "https://notebooklm.google.com/_/LabsTailwindUi/data"
	fullURL := baseURL + req.Endpoint

	// Build query parameters from urlParams with sensible defaults
	params := url.Values{}
	if _, ok := c.urlParams["bl"]; !ok {
		params.Set("bl", "boq_labs-tailwind-frontend_20250129.00_p0")
	}
	params.Set("hl", "en")
	params.Set("_reqid", c.reqid.Next())
	params.Set("rt", "c")

	// Apply all configured URL params (may override defaults)
	for k, v := range c.urlParams {
		params.Set(k, v)
	}

	fullURL = fullURL + "?" + params.Encode()

	// Encode the request body into f.req
	bodyJSON, err := json.Marshal(req.Body)
	if err != nil {
		return nil, "", fmt.Errorf("failed to encode request body: %w", err)
	}

	// Create form data
	formData := url.Values{}
	formData.Set("f.req", string(bodyJSON))
	formData.Set("at", c.authToken)

	if c.debug {
		fmt.Printf("f.req body: %s\n", string(bodyJSON))
	}

	// Create the HTTP request
	httpReq, err := http.NewRequest("POST", fullURL, strings.NewReader(formData.Encode()))
	if err != nil {
		return nil, "", fmt.Errorf("failed to create request: %w", err)
	}

	// Set default headers
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")
	httpReq.Header.Set("Cookie", c.cookies)
	httpReq.Header.Set("Origin", "https://notebooklm.google.com")
	httpReq.Header.Set("Referer", "https://notebooklm.google.com/")
	httpReq.Header.Set("X-Same-Domain", "1")
	httpReq.Header.Set("Accept", "*/*")
	httpReq.Header.Set("Accept-Language", "en-US,en;q=0.9")
	// Google extension header required for gRPC-Web endpoint authentication
	httpReq.Header.Set("x-goog-ext-353267353-jspb", "[null,null,null,282611]")

	// Apply configured headers (may override defaults)
	for k, v := range c.headers {
		httpReq.Header.Set(k, v)
	}

	return httpReq, fullURL, nil
}

// Execute sends a gRPC-style request to NotebookLM and returns raw bytes.
func (c *Client) Execute(req Request) ([]byte, error) {
	httpReq, fullURL, err := c.buildRequest(req)
	if err != nil {
		return nil, err
	}

	if c.debug {
		fmt.Printf("=== gRPC Request ===\n")
		fmt.Printf("URL: %s\n", fullURL)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if c.debug {
		fmt.Printf("=== gRPC Response ===\n")
		fmt.Printf("Status: %s\n", resp.Status)
		fmt.Printf("Body: %s\n", string(body))
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("request failed with status %d: %s", resp.StatusCode, string(body))
	}

	return body, nil
}

// ExecuteRPC sends a gRPC-style request and parses the rt=c chunked response,
// returning the inner data payload as json.RawMessage compatible with beprotojson.Unmarshal().
func (c *Client) ExecuteRPC(req Request) (json.RawMessage, error) {
	raw, err := c.Execute(req)
	if err != nil {
		return nil, err
	}

	chunks, err := c.parseChunkedResponse(raw)
	if err != nil {
		return nil, err
	}

	// Extract wrb.fr response from chunks.
	// For streamed endpoints, multiple wrb.fr entries may contain partial data.
	var allData []json.RawMessage
	for i, chunk := range chunks {
		// Try to parse as JSON array of arrays: [["wrb.fr", rpcId, data, ...], ...]
		var outer [][]interface{}
		if err := json.Unmarshal([]byte(chunk), &outer); err != nil {
			// Try as single array: ["wrb.fr", rpcId, data, ...]
			var single []interface{}
			if err2 := json.Unmarshal([]byte(chunk), &single); err2 != nil {
				if c.debug {
					fmt.Printf("Chunk %d: not JSON (len=%d): %s\n", i, len(chunk), truncate(chunk, 80))
				}
				continue
			}
			outer = [][]interface{}{single}
		}

		for _, entry := range outer {
			if len(entry) < 3 {
				continue
			}
			tag, ok := entry[0].(string)
			if !ok {
				continue
			}

			if c.debug {
				rpcID := ""
				if len(entry) > 1 {
					if s, ok := entry[1].(string); ok {
						rpcID = s
					}
				}
				dataNull := entry[2] == nil
				fmt.Printf("Chunk %d: tag=%q rpcId=%q dataNull=%v\n", i, tag, rpcID, dataNull)
			}

			if tag != "wrb.fr" {
				continue
			}
			// Check for error code in position 5: [16] = Unauthenticated
			if entry[2] == nil {
				if len(entry) > 5 {
					if errArr, ok := entry[5].([]interface{}); ok && len(errArr) > 0 {
						if code, ok := errArr[0].(float64); ok && code != 0 {
							return nil, fmt.Errorf("gRPC error code %d", int(code))
						}
					}
				}
				continue
			}
			switch data := entry[2].(type) {
			case string:
				allData = append(allData, json.RawMessage(data))
			default:
				encoded, err := json.Marshal(data)
				if err != nil {
					continue
				}
				allData = append(allData, json.RawMessage(encoded))
			}
		}
	}

	if c.debug {
		fmt.Printf("Found %d wrb.fr entries with data\n", len(allData))
	}

	if len(allData) == 0 {
		return nil, fmt.Errorf("no wrb.fr response found in chunked data")
	}

	// Return the last wrb.fr entry with data (for streamed responses,
	// later entries are more complete).
	return allData[len(allData)-1], nil
}

// parseChunkedResponse parses the rt=c chunked response format.
// Format: )]}'\n\n{size}\r\n{json}\r\n{size}\r\n{json}\r\n...
// The chunk sizes include the \r\n terminator, so we use a line-based scanner
// and alternate between "expect size" and "expect data" lines.
func (c *Client) parseChunkedResponse(raw []byte) ([]string, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	// Allow large chunks (up to 10MB)
	buf := make([]byte, 10*1024*1024)
	scanner.Buffer(buf, 10*1024*1024)

	var chunks []string
	expectData := false

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		// Skip )]}' prefix
		if strings.HasPrefix(line, ")]}'") {
			continue
		}

		if !expectData {
			// This should be a chunk size (numeric line)
			if _, err := strconv.Atoi(line); err == nil {
				expectData = true
				continue
			}
			// Not a number — treat as data directly
			chunks = append(chunks, line)
		} else {
			// This is the chunk data line
			chunks = append(chunks, line)
			expectData = false
		}
	}

	if c.debug {
		fmt.Printf("=== Chunked Parse ===\n")
		fmt.Printf("Found %d chunks\n", len(chunks))
	}

	if len(chunks) == 0 {
		return nil, fmt.Errorf("empty response after parsing chunks")
	}

	return chunks, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// reqIDGenerator generates sequential request IDs
type reqIDGenerator struct {
	counter int
	mu      sync.Mutex
}

func newReqIDGenerator() *reqIDGenerator {
	return &reqIDGenerator{}
}

func (g *reqIDGenerator) Next() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.counter++
	return strconv.Itoa(1000000 + g.counter)
}
