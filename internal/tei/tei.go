// Package tei 是 Hugging Face Text Embeddings Inference 的 client。
package tei

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type EmbedKind int

const rerankBatch = 32

const (
	Document EmbedKind = iota
	Query
)

type Client struct {
	EmbedURL    string
	RerankURL   string
	QueryPrefix string
	HTTP        *http.Client
	BatchSize   int
}

func New(embedURL, rerankURL, queryPrefix string, timeout time.Duration) *Client {
	return &Client{
		EmbedURL: embedURL, RerankURL: rerankURL, QueryPrefix: queryPrefix,
		HTTP: &http.Client{Timeout: timeout}, BatchSize: 16,
	}
}

func (c *Client) Embed(ctx context.Context, texts []string, kind EmbedKind) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for i := 0; i < len(texts); i += c.BatchSize {
		batch := texts[i:min(i+c.BatchSize, len(texts))]
		if kind == Query && c.QueryPrefix != "" {
			p := make([]string, len(batch))
			for j, t := range batch {
				p[j] = c.QueryPrefix + t
			}
			batch = p
		}
		var vecs [][]float32
		if err := c.post(ctx, c.EmbedURL+"/embed", map[string]any{"inputs": batch, "truncate": true, "normalize": true}, &vecs); err != nil {
			return nil, err
		}
		if len(vecs) != len(batch) {
			return nil, fmt.Errorf("tei embed: got %d vectors for %d inputs", len(vecs), len(batch))
		}
		out = append(out, vecs...)
	}
	return out, nil
}

// Rerank 回傳與 docs 同序的分數（0..1）。
func (c *Client) Rerank(ctx context.Context, query string, docs []string) ([]float32, error) {
	scores := make([]float32, len(docs))
	for off := 0; off < len(docs); off += rerankBatch { // TEI 預設單次最多 32 筆（--max-client-batch-size）
		batch := docs[off:min(off+rerankBatch, len(docs))]
		var res []struct {
			Index int     `json:"index"`
			Score float32 `json:"score"`
		}
		if err := c.post(ctx, c.RerankURL+"/rerank", map[string]any{"query": query, "texts": batch, "truncate": true}, &res); err != nil {
			return nil, err
		}
		for _, r := range res {
			if r.Index >= 0 && r.Index < len(batch) {
				scores[off+r.Index] = r.Score
			}
		}
	}
	return scores, nil
}

// post 只重試 429 與 5xx。
func (c *Client) post(ctx context.Context, url string, body any, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("%s: %s: %s", url, resp.Status, bytes.TrimSpace(b))
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%s: %s: %s", url, resp.Status, bytes.TrimSpace(b))
		}
		return json.Unmarshal(b, out)
	}
	return lastErr
}
