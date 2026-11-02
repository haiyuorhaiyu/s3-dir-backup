package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	defaultEndpoint = "https://s3.example.com"
	defaultRegion   = "us-east-1"
	defaultBucket   = "your-bucket"
	defaultPrefix   = "daily-backup"
)

type Config struct {
	Endpoint     string   `json:"endpoint"`
	Region       string   `json:"region"`
	Bucket       string   `json:"bucket"`
	Prefix       string   `json:"prefix"`
	AccessKey    string   `json:"accessKey"`
	SecretKey    string   `json:"secretKey"`
	SessionToken string   `json:"sessionToken,omitempty"`
	PathStyle    *bool    `json:"pathStyle"`
	Sources      []Source `json:"sources"`
}

type Source struct {
	Name string `json:"name"`
	Path string `json:"path"`
}
type syncResult struct {
	Files  int
	Bytes  int64
	Stale  int
	Source int
}

type s3Client struct {
	Endpoint                                         *url.URL
	Region, Bucket, AccessKey, SecretKey, SessionTok string
	PathStyle                                        bool
	HTTP                                             *http.Client
}

func (c Config) usePathStyle() bool {
	return c.PathStyle == nil || *c.PathStyle
}

type listResult struct {
	Contents []struct {
		Key string `xml:"Key"`
	} `xml:"Contents"`
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
}

func main() {
	if runtime.GOOS == "windows" {
		runGUI()
		return
	}
	runCLI()
}

func runCLI() {
	configPath := flag.String("config", "backup.json", "JSON configuration file")
	once := flag.Bool("once", false, "run one backup and exit")
	daily := flag.Bool("daily", false, "run immediately, then repeat at local midnight")
	check := flag.Bool("check", false, "check S3 access by listing the configured prefix without changing objects")
	flag.Parse()
	if (*once && *daily) || (*check && *daily) {
		log.Fatal("-once and -daily cannot be used together")
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *daily {
		runDaily(ctx, cfg)
		return
	}
	if *check {
		count, err := checkStorage(ctx, cfg)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("S3 access check passed: %d objects under prefix", count)
		return
	}
	result, err := syncOnce(ctx, cfg)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("backup complete: sources=%d files=%d bytes=%s stale_deleted=%d", result.Source, result.Files, formatBytes(result.Bytes), result.Stale)
}

func checkStorage(ctx context.Context, cfg Config) (int, error) {
	if err := validateConnectionConfig(cfg); err != nil {
		return 0, err
	}
	prefix, err := remotePrefix(cfg.Prefix)
	if err != nil {
		return 0, err
	}
	client, err := newClient(cfg)
	if err != nil {
		return 0, err
	}
	keys, err := client.list(ctx, prefix+"/")
	if err != nil {
		return 0, fmt.Errorf("check S3 access: %w", err)
	}
	return len(keys), nil
}

func loadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}
	cfg := Config{}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %q: %w", path, err)
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = defaultEndpoint
	}
	if cfg.Region == "" {
		cfg.Region = defaultRegion
	}
	if cfg.Bucket == "" {
		cfg.Bucket = defaultBucket
	}
	if cfg.Prefix == "" {
		cfg.Prefix = defaultPrefix
	}
	if value := os.Getenv("S3_BACKUP_ENDPOINT"); value != "" {
		cfg.Endpoint = value
	}
	if value := os.Getenv("S3_BACKUP_REGION"); value != "" {
		cfg.Region = value
	}
	if value := os.Getenv("S3_BACKUP_BUCKET"); value != "" {
		cfg.Bucket = value
	}
	if value := os.Getenv("S3_BACKUP_ACCESS_KEY"); value != "" {
		cfg.AccessKey = value
	}
	if value := os.Getenv("S3_BACKUP_SECRET_KEY"); value != "" {
		cfg.SecretKey = value
	}
	if value := os.Getenv("S3_BACKUP_SESSION_TOKEN"); value != "" {
		cfg.SessionToken = value
	}
	configDir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return Config{}, fmt.Errorf("resolve config directory: %w", err)
	}
	for i := range cfg.Sources {
		if !filepath.IsAbs(cfg.Sources[i].Path) {
			cfg.Sources[i].Path = filepath.Join(configDir, cfg.Sources[i].Path)
		}
	}
	return cfg, nil
}

func validateConfig(cfg Config) error {
	if err := validateConnectionConfig(cfg); err != nil {
		return err
	}
	seenNames := make(map[string]struct{}, len(cfg.Sources))
	for _, source := range cfg.Sources {
		if err := validateSourceName(source.Name); err != nil {
			return err
		}
		if _, exists := seenNames[source.Name]; exists {
			return fmt.Errorf("duplicate source name %q", source.Name)
		}
		seenNames[source.Name] = struct{}{}
		info, err := os.Stat(source.Path)
		if err != nil {
			return fmt.Errorf("source %q: %w", source.Path, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("source %q is not a directory", source.Path)
		}
	}
	if len(cfg.Sources) == 0 {
		return errors.New("at least one source directory is required")
	}
	return nil
}

func validateConnectionConfig(cfg Config) error {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return errors.New("endpoint is required")
	}
	if strings.TrimSpace(cfg.Region) == "" {
		return errors.New("region is required")
	}
	if strings.TrimSpace(cfg.Bucket) == "" {
		return errors.New("bucket is required")
	}
	if strings.TrimSpace(cfg.Prefix) == "" {
		return errors.New("prefix must not be empty; use a dedicated prefix")
	}
	if strings.TrimSpace(cfg.AccessKey) == "" || strings.TrimSpace(cfg.SecretKey) == "" {
		return errors.New("accessKey and secretKey are required; use JSON or S3_BACKUP_ACCESS_KEY/S3_BACKUP_SECRET_KEY")
	}
	if _, err := remotePrefix(cfg.Prefix); err != nil {
		return err
	}
	return nil
}

func newClient(cfg Config) (*s3Client, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("endpoint must be an http or https URL: %q", cfg.Endpoint)
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("endpoint must not contain a path, query, or fragment: %q", cfg.Endpoint)
	}
	return &s3Client{Endpoint: u, Region: cfg.Region, Bucket: cfg.Bucket, AccessKey: cfg.AccessKey, SecretKey: cfg.SecretKey, SessionTok: cfg.SessionToken, PathStyle: cfg.usePathStyle(), HTTP: &http.Client{Timeout: 30 * time.Minute}}, nil
}

func syncOnce(ctx context.Context, cfg Config) (syncResult, error) {
	return syncOnceProgress(ctx, cfg, nil)
}

func syncOnceProgress(ctx context.Context, cfg Config, progress func(string)) (syncResult, error) {
	if err := validateConfig(cfg); err != nil {
		return syncResult{}, err
	}
	prefix, err := remotePrefix(cfg.Prefix)
	if err != nil {
		return syncResult{}, err
	}
	client, err := newClient(cfg)
	if err != nil {
		return syncResult{}, err
	}
	remoteKeys, err := client.list(ctx, prefix+"/")
	if err != nil {
		return syncResult{}, fmt.Errorf("list remote prefix %q: %w", prefix, err)
	}
	desiredKeys := make(map[string]struct{})
	result := syncResult{Source: len(cfg.Sources)}
	lastProgress := time.Time{}
	for _, source := range cfg.Sources {
		root, err := filepath.Abs(source.Path)
		if err != nil {
			return syncResult{}, fmt.Errorf("resolve source %q: %w", source.Path, err)
		}
		err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			if entry.Type()&os.ModeSymlink != 0 {
				log.Printf("skip symlink: %s", path)
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return nil
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			key := objectKey(prefix, source.Name, relative)
			if progress != nil && (lastProgress.IsZero() || time.Since(lastProgress) >= time.Second) {
				progress(fmt.Sprintf("正在备份 %s：%s", source.Name, relative))
				lastProgress = time.Now()
			}
			if err := client.putFile(ctx, key, path, info.Size(), mime.TypeByExtension(filepath.Ext(path))); err != nil {
				return fmt.Errorf("upload %q: %w", path, err)
			}
			desiredKeys[key] = struct{}{}
			result.Files++
			result.Bytes += info.Size()
			return nil
		})
		if err != nil {
			return syncResult{}, fmt.Errorf("scan source %q: %w", source.Path, err)
		}
	}
	for key := range remoteKeys {
		if _, keep := desiredKeys[key]; keep {
			continue
		}
		if err := client.remove(ctx, key); err != nil {
			return syncResult{}, fmt.Errorf("delete stale object %q: %w", key, err)
		}
		result.Stale++
	}
	return result, nil
}

func (c *s3Client) list(ctx context.Context, prefix string) (map[string]struct{}, error) {
	keys := make(map[string]struct{})
	var token string
	for {
		query := url.Values{"list-type": {"2"}, "prefix": {prefix}, "max-keys": {"1000"}}
		if token != "" {
			query.Set("continuation-token", token)
		}
		body, err := c.request(ctx, http.MethodGet, "", query, nil, 0, "", sha256Hex(nil))
		if err != nil {
			return nil, err
		}
		var result listResult
		if err := xml.Unmarshal(body, &result); err != nil {
			return nil, fmt.Errorf("parse list response: %w", err)
		}
		for _, item := range result.Contents {
			keys[item.Key] = struct{}{}
		}
		if !result.IsTruncated || result.NextContinuationToken == "" {
			return keys, nil
		}
		token = result.NextContinuationToken
	}
}

func (c *s3Client) putFile(ctx context.Context, key, path string, size int64, contentType string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	_, err = c.request(ctx, http.MethodPut, key, nil, file, size, contentType, hex.EncodeToString(hash.Sum(nil)))
	return err
}

func (c *s3Client) remove(ctx context.Context, key string) error {
	_, err := c.request(ctx, http.MethodDelete, key, nil, nil, 0, "", sha256Hex(nil))
	return err
}

func (c *s3Client) request(ctx context.Context, method, key string, query url.Values, body io.Reader, contentLength int64, contentType, payloadHash string) ([]byte, error) {
	host := c.Endpoint.Host
	path := "/" + awsEncode(c.Bucket, true)
	if !c.PathStyle {
		host = c.Bucket + "." + host
		path = "/"
	}
	if key != "" {
		if c.PathStyle {
			path += "/"
		}
		path += awsEncode(key, false)
	}
	canonicalQuery := canonicalQuery(query)
	requestURL := c.Endpoint.Scheme + "://" + host + path
	if canonicalQuery != "" {
		requestURL += "?" + canonicalQuery
	}
	req, err := http.NewRequestWithContext(ctx, method, requestURL, body)
	if err != nil {
		return nil, err
	}
	req.Host = host
	req.ContentLength = contentLength
	req.Header.Set("Host", host)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	date := now.Format("20060102")
	req.Header.Set("x-amz-date", amzDate)
	if c.SessionTok != "" {
		req.Header.Set("x-amz-security-token", c.SessionTok)
	}
	canonicalHeaders, signedHeadersValue := signedHeaders(req)
	canonicalRequest := strings.Join([]string{method, path, canonicalQuery, canonicalHeaders, signedHeadersValue, payloadHash}, "\n")
	scope := date + "/" + c.Region + "/s3/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + sha256Hex([]byte(canonicalRequest))
	signature := hmacHex(signingKey(c.SecretKey, date, c.Region, "s3"), stringToSign)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+c.AccessKey+"/"+scope+", SignedHeaders="+signedHeadersValue+", Signature="+signature)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if readErr != nil {
		return nil, readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("S3 request failed: HTTP %s: %s", resp.Status, strings.TrimSpace(string(responseBody)))
	}
	return responseBody, nil
}

func runDaily(ctx context.Context, cfg Config) {
	for {
		result, err := syncOnce(ctx, cfg)
		if err != nil {
			log.Printf("backup failed: %v", err)
		} else {
			log.Printf("backup complete: sources=%d files=%d bytes=%s stale_deleted=%d", result.Source, result.Files, formatBytes(result.Bytes), result.Stale)
		}
		now := time.Now()
		tomorrow := now.AddDate(0, 0, 1)
		next := time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 0, 0, 0, 0, tomorrow.Location())
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
	}
}

func objectKey(prefix, sourceName, relativePath string) string {
	return strings.Join([]string{prefix, sourceName, filepath.ToSlash(relativePath)}, "/")
}
func remotePrefix(value string) (string, error) {
	prefix := strings.Trim(strings.ReplaceAll(value, "\\", "/"), "/")
	if prefix == "" {
		return "", errors.New("prefix must not be empty")
	}
	for _, part := range strings.Split(prefix, "/") {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("invalid prefix %q", value)
		}
	}
	return prefix, nil
}
func validateSourceName(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("source name is required")
	}
	if name == "." || name == ".." || strings.ContainsAny(name, `/\\`) {
		return fmt.Errorf("source name must be a single path component: %q", name)
	}
	return nil
}
func formatBytes(value int64) string {
	if value < 1024 {
		return fmt.Sprintf("%d B", value)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	amount := float64(value)
	for _, unit := range units {
		amount /= 1024
		if amount < 1024 {
			return fmt.Sprintf("%.1f %s", amount, unit)
		}
	}
	return fmt.Sprintf("%.1f PB", amount/1024)
}
func sha256Hex(data []byte) string { hash := sha256.Sum256(data); return hex.EncodeToString(hash[:]) }
func hmacHex(key []byte, value string) string {
	hash := hmac.New(sha256.New, key)
	_, _ = hash.Write([]byte(value))
	return hex.EncodeToString(hash.Sum(nil))
}
func signingKey(secret, date, region, service string) []byte {
	kDate := hmac.New(sha256.New, []byte("AWS4"+secret))
	_, _ = kDate.Write([]byte(date))
	kRegion := hmac.New(sha256.New, kDate.Sum(nil))
	_, _ = kRegion.Write([]byte(region))
	kService := hmac.New(sha256.New, kRegion.Sum(nil))
	_, _ = kService.Write([]byte(service))
	kSigning := hmac.New(sha256.New, kService.Sum(nil))
	_, _ = kSigning.Write([]byte("aws4_request"))
	return kSigning.Sum(nil)
}

func signedHeaders(req *http.Request) (string, string) {
	values := map[string]string{"host": req.Host, "x-amz-content-sha256": req.Header.Get("x-amz-content-sha256"), "x-amz-date": req.Header.Get("x-amz-date")}
	if value := req.Header.Get("content-type"); value != "" {
		values["content-type"] = value
	}
	if value := req.Header.Get("x-amz-security-token"); value != "" {
		values["x-amz-security-token"] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var canonical strings.Builder
	for _, key := range keys {
		canonical.WriteString(key)
		canonical.WriteByte(':')
		canonical.WriteString(strings.Join(strings.Fields(values[key]), " "))
		canonical.WriteByte('\n')
	}
	return canonical.String(), strings.Join(keys, ";")
}
func canonicalQuery(values url.Values) string {
	if len(values) == 0 {
		return ""
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var parts []string
	for _, key := range keys {
		items := append([]string(nil), values[key]...)
		sort.Strings(items)
		if len(items) == 0 {
			items = []string{""}
		}
		for _, item := range items {
			parts = append(parts, awsEncode(key, true)+"="+awsEncode(item, true))
		}
	}
	return strings.Join(parts, "&")
}
func awsEncode(value string, encodeSlash bool) string {
	const hexChars = "0123456789ABCDEF"
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_' || ch == '.' || ch == '~' || (ch == '/' && !encodeSlash) {
			out.WriteByte(ch)
		} else {
			out.WriteByte('%')
			out.WriteByte(hexChars[ch>>4])
			out.WriteByte(hexChars[ch&15])
		}
	}
	return out.String()
}
