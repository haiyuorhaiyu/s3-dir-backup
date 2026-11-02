//go:build windows

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

func checkS5cmdStorage(ctx context.Context, cfg Config, progress func(string)) error {
	if err := validateConnectionConfig(cfg); err != nil {
		return err
	}
	credentialsPath, err := writeS5cmdCredentials(cfg)
	if err != nil {
		return err
	}
	defer os.Remove(credentialsPath)
	_, err = runS5cmd(ctx, credentialsPath, cfg, progress, "head", "s3://"+cfg.Bucket)
	if err != nil {
		return fmt.Errorf("S3 access check: %w", err)
	}
	return nil
}

func syncWithS5cmd(ctx context.Context, cfg Config, progress func(string)) (syncResult, error) {
	if err := validateConnectionConfig(cfg); err != nil {
		return syncResult{}, err
	}
	if len(cfg.Sources) == 0 {
		return syncResult{}, errors.New("at least one source directory is required")
	}
	seen := make(map[string]struct{}, len(cfg.Sources))
	for _, source := range cfg.Sources {
		if err := validateSourceName(source.Name); err != nil {
			return syncResult{}, err
		}
		if _, ok := seen[source.Name]; ok {
			return syncResult{}, fmt.Errorf("duplicate source name %q", source.Name)
		}
		seen[source.Name] = struct{}{}
		info, err := os.Stat(source.Path)
		if err != nil {
			return syncResult{}, fmt.Errorf("source %q: %w", source.Path, err)
		}
		if !info.IsDir() {
			return syncResult{}, fmt.Errorf("source %q is not a directory", source.Path)
		}
	}
	credentialsPath, err := writeS5cmdCredentials(cfg)
	if err != nil {
		return syncResult{}, err
	}
	defer os.Remove(credentialsPath)
	result := syncResult{Source: len(cfg.Sources)}
	for _, source := range cfg.Sources {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if progress != nil {
			progress("正在同步目录：" + source.Path)
		}
		local := filepath.Clean(source.Path)
		if !strings.HasSuffix(local, string(os.PathSeparator)) {
			local += string(os.PathSeparator)
		}
		remote := "s3://" + remotePath(cfg, source.Name) + "/"
		_, err := runS5cmd(ctx, credentialsPath, cfg, progress,
			"sync", "--raw", "--delete", "--concurrency", "4", "--part-size", "16", local, remote)
		if err != nil {
			return result, fmt.Errorf("sync directory %q: %w", source.Path, err)
		}
	}
	return result, nil
}

func remotePath(cfg Config, source string) string {
	prefix := strings.Trim(strings.ReplaceAll(cfg.Prefix, "\\", "/"), "/")
	parts := []string{cfg.Bucket, prefix}
	if source != "" {
		parts = append(parts, source)
	}
	return strings.Join(parts, "/")
}

func writeS5cmdCredentials(cfg Config) (string, error) {
	for _, value := range []string{cfg.AccessKey, cfg.SecretKey, cfg.SessionToken} {
		if strings.ContainsAny(value, "\r\n") {
			return "", errors.New("S3 credentials cannot contain line breaks")
		}
	}
	content := "[default]\naws_access_key_id = " + cfg.AccessKey + "\naws_secret_access_key = " + cfg.SecretKey + "\n"
	if cfg.SessionToken != "" {
		content += "aws_session_token = " + cfg.SessionToken + "\n"
	}
	file, err := os.CreateTemp("", "s3-dir-backup-credentials-*.ini")
	if err != nil {
		return "", fmt.Errorf("create temporary credentials file: %w", err)
	}
	path := file.Name()
	if _, err := file.WriteString(content); err != nil {
		file.Close()
		os.Remove(path)
		return "", fmt.Errorf("write temporary credentials file: %w", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("close temporary credentials file: %w", err)
	}
	return path, nil
}

func runS5cmd(ctx context.Context, credentialsPath string, cfg Config, progress func(string), args ...string) (string, error) {
	exePath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate application: %w", err)
	}
	s5cmdPath := filepath.Join(filepath.Dir(exePath), "s5cmd.exe")
	if _, err := os.Stat(s5cmdPath); err != nil {
		return "", fmt.Errorf("s5cmd.exe must be beside the backup application: %w", err)
	}
	fullArgs := []string{"--credentials-file", credentialsPath, "--endpoint-url", cfg.Endpoint, "--retry-count", "5", "--numworkers", "16"}
	fullArgs = append(fullArgs, args...)
	cmd := exec.CommandContext(ctx, s5cmdPath, fullArgs...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.Env = make([]string, 0, len(os.Environ())+2)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !strings.EqualFold(name, "AWS_REGION") && !strings.EqualFold(name, "AWS_DEFAULT_REGION") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "AWS_REGION="+cfg.Region, "AWS_DEFAULT_REGION="+cfg.Region)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start s5cmd: %w", err)
	}
	var mu sync.Mutex
	var output strings.Builder
	var wg sync.WaitGroup
	var lastProgress time.Time
	read := func(stream io.ReadCloser) {
		defer wg.Done()
		scanner := bufio.NewScanner(stream)
		scanner.Buffer(make([]byte, 4096), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			mu.Lock()
			if output.Len() < 2*1024*1024 {
				output.WriteString(line)
				output.WriteByte('\n')
			}
			mu.Unlock()
			if progress != nil && strings.TrimSpace(line) != "" {
				mu.Lock()
				now := time.Now()
				if now.Sub(lastProgress) >= 1500*time.Millisecond {
					lastProgress = now
					mu.Unlock()
					progress(line)
				} else {
					mu.Unlock()
				}
			}
		}
	}
	wg.Add(2)
	go read(stdout)
	go read(stderr)
	waitErr := cmd.Wait()
	wg.Wait()
	mu.Lock()
	result := output.String()
	mu.Unlock()
	if waitErr != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if strings.TrimSpace(result) != "" {
			return result, fmt.Errorf("%w: %s", waitErr, strings.TrimSpace(result))
		}
		return result, waitErr
	}
	return result, nil
}
