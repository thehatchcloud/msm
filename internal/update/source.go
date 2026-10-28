package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Source fetches release files by name. Every implementation must treat the
// bytes as untrusted: the signature, not the transport, is the authority.
type Source interface {
	Open(ctx context.Context, name string) (io.ReadCloser, error)
}

// HTTPSource fetches files from an https directory URL.
type HTTPSource struct {
	base   *url.URL
	client *http.Client
}

// NewHTTPSource requires an https base. client may be nil; its redirects are
// always limited to https and to a few hops.
func NewHTTPSource(base string, client *http.Client) (*HTTPSource, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return nil, fmt.Errorf("update: release location %q must be an https URL without credentials", base)
	}
	c := http.Client{Timeout: 15 * time.Minute}
	if client != nil {
		c = *client
	}
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("update: too many redirects")
		}
		if req.URL.Scheme != "https" {
			return fmt.Errorf("update: refusing redirect to %s", req.URL.Scheme)
		}
		return nil
	}
	return &HTTPSource{base: u, client: &c}, nil
}

func (s *HTTPSource) Open(ctx context.Context, name string) (io.ReadCloser, error) {
	if !fileRE.MatchString(name) {
		return nil, fmt.Errorf("update: bad file name %q", name)
	}
	u := *s.base
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + name
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("update: fetch %s: %w", name, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("update: fetch %s: %s", name, resp.Status)
	}
	return resp.Body, nil
}

// DirSource reads release files from a local directory (an unpacked release,
// for tests and offline installs). The same verification applies.
type DirSource string

func (d DirSource) Open(_ context.Context, name string) (io.ReadCloser, error) {
	if !fileRE.MatchString(name) {
		return nil, fmt.Errorf("update: bad file name %q", name)
	}
	return os.Open(filepath.Join(string(d), name))
}

// readLimited reads a whole small file from src.
func readLimited(ctx context.Context, src Source, name string, limit int64) ([]byte, error) {
	rc, err := src.Open(ctx, name)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, fmt.Errorf("update: read %s: %w", name, err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("update: %s is larger than %d bytes", name, limit)
	}
	return b, nil
}
