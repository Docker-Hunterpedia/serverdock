package dockerapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/Docker-Hunterpedia/serverdock/internal/agent"
)

// maxRefLen bounds an image reference (Docker's own limit is 255 plus a tag).
const maxRefLen = 255 + 1 + 128

// validRef is the client's own syntax gate for a reference or image id that
// goes into a request path or query: printable ASCII from the reference
// alphabet only. The full grammar and the registry-host rules belong to the
// policy package.
func validRef(ref string) bool {
	if ref == "" || len(ref) > maxRefLen || strings.Contains(ref, "..") || strings.Contains(ref, "//") {
		return false
	}
	for i := 0; i < len(ref); i++ {
		c := ref[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == '-' || c == ':' || c == '/' || c == '@':
		default:
			return false
		}
	}
	return ref[0] != '/' && ref[0] != '-'
}

// PullProgress is the running total of one pull, built from Docker's per-layer
// lines. Layer ids are tracked up to maxLayers.
type PullProgress struct {
	Layers     int
	LayersDone int
	Bytes      int64
	BytesTotal int64
	Status     string // the latest status line, cleaned and capped at 256 bytes
}

const maxLayers = 512

type layerState struct {
	current, total int64
	done           bool
}

// ImagePull pulls ref and reports progress through onProgress (called on the
// goroutine of the caller, once per Docker line; the caller rate-limits). It
// returns when Docker ends the stream. A stream that reports an error (an
// `error` or `errorDetail` line) fails the pull; "no space left on device"
// matches ErrDiskFull. The call is bounded by ctx only (pulls are long); the
// caller adds its own stall watch by cancelling ctx.
func (c *Client) ImagePull(ctx context.Context, ref string, onProgress func(PullProgress)) error {
	if !validRef(ref) {
		return errors.New("dockerapi: invalid image reference")
	}
	resp, err := c.stream(ctx, http.MethodPost, "/images/create?fromImage="+url.QueryEscape(ref), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	layers := map[string]*layerState{}
	var p PullProgress
	lines := newJSONLines(resp.Body)
	for {
		line, err := lines.next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("dockerapi: pull: %w", err)
		}
		var m struct {
			Status         string                    `json:"status"`
			ID             string                    `json:"id"`
			Error          string                    `json:"error"`
			ErrorDetail    *struct{ Message string } `json:"errorDetail"`
			ProgressDetail *struct {
				Current int64 `json:"current"`
				Total   int64 `json:"total"`
			} `json:"progressDetail"`
		}
		if err := json.Unmarshal(line, &m); err != nil {
			return fmt.Errorf("dockerapi: pull: bad progress line: %w", err)
		}
		if m.Error != "" || m.ErrorDetail != nil {
			msg := m.Error
			if m.ErrorDetail != nil && m.ErrorDetail.Message != "" {
				msg = m.ErrorDetail.Message
			}
			if isNoSpace(msg) {
				return fmt.Errorf("%w: %s", ErrDiskFull, agent.CleanText(msg, maxErrMsg))
			}
			return fmt.Errorf("dockerapi: pull: %s", agent.CleanText(msg, maxErrMsg))
		}
		p.Status = agent.CleanText(m.Status, maxErrMsg)
		// Only lines for a layer id (not the "Pulling from" or "Digest" lines).
		if m.ID != "" && len(m.ID) <= 80 && (m.ProgressDetail != nil || isLayerStatus(m.Status)) {
			l := layers[m.ID]
			if l == nil && len(layers) < maxLayers {
				l = &layerState{}
				layers[m.ID] = l
			}
			if l != nil {
				if m.ProgressDetail != nil && m.ProgressDetail.Total > 0 {
					l.current, l.total = max(m.ProgressDetail.Current, 0), m.ProgressDetail.Total
				}
				switch m.Status {
				case "Pull complete", "Already exists", "Download complete":
					l.done = true
					if l.total > 0 {
						l.current = l.total
					}
				}
			}
		}
		p.Layers, p.LayersDone, p.Bytes, p.BytesTotal = 0, 0, 0, 0
		for _, l := range layers {
			p.Layers++
			if l.done {
				p.LayersDone++
			}
			p.Bytes += l.current
			p.BytesTotal += l.total
		}
		if onProgress != nil {
			onProgress(p)
		}
	}
}

func isLayerStatus(s string) bool {
	switch s {
	case "Pulling fs layer", "Waiting", "Downloading", "Verifying Checksum", "Download complete",
		"Extracting", "Pull complete", "Already exists":
		return true
	}
	return false
}

// ImageInfo is what the deploy path needs from an inspected image. Label
// values are never returned; ReservedLabelKeys lists the label keys that start
// with "serverdock." so the policy can refuse a spoofing image.
type ImageInfo struct {
	ID                string
	RepoDigests       []string
	ExposedPorts      []string // "8080/tcp", sorted
	Volumes           []string // sorted
	ReservedLabelKeys []string // sorted
}

// ImageInspect inspects an image by reference or id.
func (c *Client) ImageInspect(ctx context.Context, ref string) (ImageInfo, error) {
	if !validRef(ref) {
		return ImageInfo{}, errors.New("dockerapi: invalid image reference")
	}
	st, body, err := c.call(ctx, c.short, http.MethodGet, "/images/"+ref+"/json", nil)
	if err != nil {
		return ImageInfo{}, err
	}
	if err := statusErr("image inspect", st, body, http.StatusOK); err != nil {
		return ImageInfo{}, err
	}
	var r struct {
		ID          string
		RepoDigests []string
		Config      *struct {
			ExposedPorts map[string]json.RawMessage
			Volumes      map[string]json.RawMessage
			Labels       map[string]json.RawMessage
		}
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return ImageInfo{}, fmt.Errorf("dockerapi: image inspect: %w", err)
	}
	info := ImageInfo{ID: r.ID, RepoDigests: r.RepoDigests}
	if r.Config != nil {
		info.ExposedPorts = sortedKeys(r.Config.ExposedPorts)
		info.Volumes = sortedKeys(r.Config.Volumes)
		for k := range r.Config.Labels {
			if strings.HasPrefix(strings.ToLower(k), "serverdock.") {
				info.ReservedLabelKeys = append(info.ReservedLabelKeys, k)
			}
		}
		sort.Strings(info.ReservedLabelKeys)
	}
	return info, nil
}

func sortedKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
