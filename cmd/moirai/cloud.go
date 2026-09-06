package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	moirai "github.com/october-dev/moirai"
	"github.com/october-dev/moirai/internal/sharing"
)

type cloudConfig struct {
	Server string `json:"server"`
	Token  string `json:"token"`
}
type cloudClient struct {
	config cloudConfig
	http   *http.Client
}

func configPath() (string, error) {
	if p := os.Getenv("MOIRAI_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	return filepath.Join(dir, "moirai", "cloud.json"), err
}
func loadCloud(server string) (*cloudClient, error) {
	cfg := cloudConfig{Server: "https://moirai.to"}
	path, err := configPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err == nil {
		if err = json.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("invalid cloud configuration: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	override := first(server, os.Getenv("MOIRAI_SERVER"))
	if override != "" && strings.TrimRight(override, "/") != strings.TrimRight(cfg.Server, "/") {
		cfg.Token = ""
		cfg.Server = override
	}
	if token := os.Getenv("MOIRAI_TOKEN"); token != "" {
		cfg.Token = token
	}
	cfg.Server = strings.TrimRight(cfg.Server, "/")
	u, err := url.Parse(cfg.Server)
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid cloud server origin")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1")) {
		return nil, errors.New("cloud requires HTTPS (HTTP allowed only on loopback)")
	}
	return &cloudClient{cfg, &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (c *cloudClient) request(ctx context.Context, method, path string, body any, key string) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.config.Server+path, reader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.config.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.config.Token)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	response, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, moirai.DefaultLimits().MaxInputBytes+1))
	if err != nil {
		return nil, response.StatusCode, err
	}
	if int64(len(data)) > moirai.DefaultLimits().MaxInputBytes {
		return nil, response.StatusCode, moirai.ErrLimitExceeded
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var message struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &message)
		return nil, response.StatusCode, fmt.Errorf("cloud HTTP %d: %s", response.StatusCode, moirai.ScrubTerminal(message.Error))
	}
	return data, response.StatusCode, nil
}
func randomKey() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
func (a app) cloudLogin(ctx context.Context, args []string) error {
	fs := newFlags("login", a.err)
	server := fs.String("server", "", "cloud origin")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("login takes no positional arguments")
	}
	c, err := loadCloud(*server)
	if err != nil {
		return err
	}
	c.config.Token = ""
	data, _, err := c.request(ctx, "POST", "/v1/auth/device", nil, "")
	if err != nil {
		return err
	}
	var device struct {
		ID    string `json:"id"`
		Token string `json:"token"`
		URI   string `json:"verification_uri"`
	}
	if err = json.Unmarshal(data, &device); err != nil {
		return err
	}
	if len(device.ID) != 48 || len(device.Token) != 48 {
		return errors.New("invalid device response")
	}
	fmt.Fprintf(a.out, "Open %s\nVerify this code in your browser: %s\n", moirai.ScrubTerminal(device.URI), device.ID)
	c.config.Token = device.Token
	deadline := time.NewTimer(10 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("login expired; run moirai login again")
		case <-ticker.C:
			_, code, err := c.request(ctx, "GET", "/v1/auth/device/"+device.ID, nil, "")
			if err != nil {
				return err
			}
			if code == 202 {
				continue
			}
			path, err := configPath()
			if err != nil {
				return err
			}
			data, err = json.MarshalIndent(c.config, "", "  ")
			if err != nil {
				return err
			}
			if err = writeOutput(path, data, a.out); err != nil {
				return err
			}
			fmt.Fprintln(a.out, "Signed in. Cloud credentials saved privately.")
			return nil
		}
	}
}
func (a app) cloudLogout(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return errors.New("logout takes no arguments")
	}
	c, err := loadCloud("")
	if err != nil {
		return err
	}
	_, _, err = c.request(ctx, "POST", "/v1/logout", nil, "")
	if err != nil {
		return err
	}
	path, err := configPath()
	if err != nil {
		return err
	}
	c.config.Token = ""
	data, _ := json.Marshal(c.config)
	return writeOutput(path, data, a.out)
}
func (a app) publish(ctx context.Context, args []string) error {
	fs := newFlags("publish", a.err)
	from := fs.String("from", "", "source format")
	visibility := fs.String("visibility", "private", "private, unlisted, or public")
	expires := fs.Duration("expires", 7*24*time.Hour, "expiry duration, 0 for no expiry")
	parent := fs.String("parent", "", "parent publication ID")
	team := fs.String("team", "", "team ID for shared ownership")
	yes := fs.Bool("yes", false, "confirm publication of reviewed content")
	preview := fs.String("preview-out", "", "write the prepared archive locally")
	includeThinking := fs.Bool("include-thinking", false, "include reasoning explicitly stored by the source")
	key := fs.String("idempotency-key", "", "advanced request key; reuse requires the identical archive, expiry and settings")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("publish requires a file or session ID")
	}
	if *visibility != "private" && *visibility != "unlisted" && *visibility != "public" {
		return errors.New("invalid visibility")
	}
	if *expires < 0 {
		return errors.New("expiry must not be negative")
	}
	var parsed *moirai.ParseResult
	var err error
	if *from != "" && !isInputFile(fs.Arg(0)) {
		parsed, err = loadStored(ctx, fs.Arg(0), moirai.Format(*from), moirai.DefaultStoreLimits())
	} else {
		parsed, _, err = parseFile(fs.Arg(0), moirai.Format(*from), moirai.DefaultLimits())
	}
	if err != nil {
		return err
	}
	a.printWarnings(parsed.Warnings)
	prepared, report, err := sharing.Prepare(parsed.Transcript, *includeThinking)
	if err != nil {
		return err
	}
	archive, err := moirai.EncodeArchive(prepared, moirai.DefaultLimits())
	if err != nil {
		return err
	}
	if int64(len(archive)) > moirai.DefaultLimits().MaxInputBytes {
		return moirai.ErrLimitExceeded
	}
	if err = writeJSON(a.err, report); err != nil {
		return err
	}
	if *preview != "" {
		if err = writeOutput(*preview, archive, a.out); err != nil {
			return err
		}
		fmt.Fprintln(a.err, "Prepared archive saved. Review the complete file before publishing.")
	}
	if !*yes {
		fmt.Fprintln(a.out, "Nothing uploaded. Review with --preview-out FILE, then publish the reviewed file with --yes.")
		return nil
	}
	c, err := loadCloud("")
	if err != nil {
		return err
	}
	if c.config.Token == "" {
		return errors.New("run moirai login before publishing")
	}
	if *key == "" {
		*key, err = randomKey()
		if err != nil {
			return err
		}
	}
	var expiry int64
	if *expires != 0 {
		expiry = time.Now().Add(*expires).Unix()
	}
	body := struct {
		Archive    json.RawMessage `json:"archive"`
		Visibility string          `json:"visibility"`
		Expires    int64           `json:"expires"`
		Parent     string          `json:"parent,omitempty"`
		Team       string          `json:"team,omitempty"`
	}{archive, *visibility, expiry, *parent, *team}
	// Retry transient failures with the same exact body and idempotency key.
	var data []byte
	var code int
	for attempt := 0; attempt < 3; attempt++ {
		data, code, err = c.request(ctx, "POST", "/v1/publications", body, *key)
		if err == nil {
			break
		}
		if code != 0 && code < 500 {
			break
		}
	}
	if err != nil {
		return err
	}
	_, err = a.out.Write(append(data, '\n'))
	return err
}
func (c *cloudClient) publicationID(value string) (string, error) {
	id := value
	if strings.Contains(value, "://") {
		u, err := url.Parse(value)
		if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Scheme+"://"+u.Host != c.config.Server || !strings.HasPrefix(u.Path, "/s/") {
			return "", errors.New("share URL must match the configured cloud server; use moirai login --server ORIGIN")
		}
		id = strings.TrimPrefix(u.Path, "/s/")
	}
	if len(id) != 48 {
		return "", errors.New("invalid publication ID")
	}
	if _, err := hex.DecodeString(id); err != nil {
		return "", errors.New("invalid publication ID")
	}
	return id, nil
}
func (a app) pull(ctx context.Context, args []string) error {
	fs := newFlags("pull", a.err)
	out := fs.String("out", "", "destination .moirai file")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 || *out == "" {
		return errors.New("pull requires a share URL or ID and --out")
	}
	c, err := loadCloud("")
	if err != nil {
		return err
	}
	id, err := c.publicationID(fs.Arg(0))
	if err != nil {
		return err
	}
	data, _, err := c.request(ctx, "GET", "/v1/publications/"+id+"/archive", nil, "")
	if err != nil {
		return err
	}
	if _, err = moirai.DecodeArchive(data, moirai.DefaultLimits()); err != nil {
		return err
	}
	if err = writeNewArchive(*out, data); err != nil {
		return err
	}
	fmt.Fprintln(a.err, "Downloaded and integrity-checked. Review before continuing; prepare the destination workspace separately.")
	return nil
}

// Link a complete private temporary file into place without replacing an
// existing destination, including one created concurrently or a dangling link.
func writeNewArchive(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".moirai-download-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Link(f.Name(), path); os.IsExist(err) {
		return errors.New("destination exists; choose a new --out path")
	}
	return err
}
func (a app) cloudMutation(ctx context.Context, op string, args []string) error {
	fs := newFlags(op, a.err)
	yes := fs.Bool("yes", false, "confirm access revocation or deletion")
	login := fs.String("login", "", "GitHub account to invite")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("%s requires a publication ID", op)
	}
	c, err := loadCloud("")
	if err != nil {
		return err
	}
	id, err := c.publicationID(fs.Arg(0))
	if err != nil {
		return err
	}
	method, path := "POST", "/v1/publications/"+id
	var body any
	switch op {
	case "invite":
		if *login == "" {
			return errors.New("invite requires --login")
		}
		path += "/grants"
		body = map[string]string{"login": *login}
	case "unpublish":
		if !*yes {
			return errors.New("unpublish requires --yes; downloaded copies cannot be recalled")
		}
		path += "/revoke"
	case "cloud-delete":
		if !*yes {
			return errors.New("cloud-delete requires --yes")
		}
		method = "DELETE"
	}
	_, _, err = c.request(ctx, method, path, body, "")
	return err
}

func (a app) fork(ctx context.Context, args []string) error {
	fs := newFlags("fork", a.err)
	yes := fs.Bool("yes", false, "create a private hosted copy with parent ancestry")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 || !*yes {
		return errors.New("fork requires a share URL or ID and --yes")
	}
	c, err := loadCloud("")
	if err != nil {
		return err
	}
	id, err := c.publicationID(fs.Arg(0))
	if err != nil {
		return err
	}
	key, err := randomKey()
	if err != nil {
		return err
	}
	var data []byte
	for i := 0; i < 3; i++ {
		var code int
		data, code, err = c.request(ctx, "POST", "/v1/publications/"+id+"/fork", nil, key)
		if err == nil || code != 0 && code < 500 {
			break
		}
	}
	if err != nil {
		return err
	}
	_, err = a.out.Write(append(data, '\n'))
	return err
}
