// Package client talks to a protobuffet registry over HTTP.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/smoreg/protobuffet/internal/api"
	"github.com/smoreg/protobuffet/internal/registry"
)

// Client is a registry API client.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// New builds a client against a registry root URL.
func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 2 * time.Minute},
	}
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusConflict {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return nil, fmt.Errorf("%s %s: %s: %s", req.Method, req.URL.Path, resp.Status, strings.TrimSpace(string(body)))
	}
	return resp, nil
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

// ModuleSummary is one row of the module listing.
type ModuleSummary struct {
	Module   string    `json:"module"`
	Versions int       `json:"versions"`
	Latest   string    `json:"latest"`
	Updated  time.Time `json:"updated"`
}

// ListModules returns every module in the registry, following pagination.
func (c *Client) ListModules(ctx context.Context) ([]ModuleSummary, error) {
	var all []ModuleSummary
	cursor := ""
	for {
		var page struct {
			Modules []ModuleSummary `json:"modules"`
			Next    string          `json:"next"`
		}
		path := "/api/v1/modules?limit=1000"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		if err := c.getJSON(ctx, path, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Modules...)
		if page.Next == "" {
			return all, nil
		}
		cursor = page.Next
	}
}

// Module returns a module index.
func (c *Client) Module(ctx context.Context, name string) (*registry.Index, error) {
	var out registry.Index
	if err := c.getJSON(ctx, "/api/v1/modules/"+name, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SchemaURL is the address buf can read a published schema archive from,
// suitable for `buf breaking --against`.
func (c *Client) SchemaURL(module, version string) string {
	if version == "" {
		version = "latest"
	}
	return fmt.Sprintf("%s/api/v1/modules/%s/versions/%s/schema.zip", c.BaseURL, module, url.PathEscape(version))
}

// DownloadSchema fetches a published schema archive.
func (c *Client) DownloadSchema(ctx context.Context, module, version string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.SchemaURL(module, version), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// Publish uploads a version. A registry that refuses the push because it
// breaks compatibility returns the report with ok=false rather than an error,
// because that is a verdict, not a failure.
func (c *Client) Publish(ctx context.Context, man api.PublishManifest, files map[string][]byte) (*api.PublishResponse, bool, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)

	manifestJSON, err := json.Marshal(man)
	if err != nil {
		return nil, false, err
	}
	if err := mw.WriteField("manifest", string(manifestJSON)); err != nil {
		return nil, false, err
	}
	for name, content := range files {
		w, err := mw.CreateFormFile(name, name)
		if err != nil {
			return nil, false, err
		}
		if _, err := w.Write(content); err != nil {
			return nil, false, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, false, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/api/v1/modules/%s/versions", c.BaseURL, man.Module), &body)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()

	var out api.PublishResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, false, fmt.Errorf("decode response: %w", err)
	}
	return &out, resp.StatusCode == http.StatusCreated, nil
}

// SetYanked marks a version as yanked, or clears the mark.
func (c *Client) SetYanked(ctx context.Context, module, version string, yanked bool, reason string) error {
	body, err := json.Marshal(map[string]string{"reason": reason})
	if err != nil {
		return err
	}
	method := http.MethodPut
	if !yanked {
		method = http.MethodDelete
	}
	req, err := http.NewRequestWithContext(ctx, method,
		fmt.Sprintf("%s/api/v1/modules/%s/versions/%s/yank", c.BaseURL, module, url.PathEscape(version)),
		bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// SetLabel points a label at a version.
func (c *Client) SetLabel(ctx context.Context, module, label, version string) error {
	body, err := json.Marshal(map[string]string{"version": version})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		fmt.Sprintf("%s/api/v1/modules/%s/labels/%s", c.BaseURL, module, label), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}
