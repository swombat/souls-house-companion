package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultLatestReleaseURL = "https://api.github.com/repos/swombat/souls-house-companion/releases/latest"
	maxReleaseMetaBytes     = 1 << 20
	maxChecksumsBytes       = 256 << 10
	maxArchiveBytes         = 128 << 20
	maxExtractedFileBytes   = 64 << 20
)

var allowedReleaseHosts = map[string]bool{
	"api.github.com":                        true,
	"github.com":                            true,
	"objects.githubusercontent.com":         true,
	"release-assets.githubusercontent.com":  true,
	"github-releases.githubusercontent.com": true,
}

type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type releaseInfo struct {
	TagName string         `json:"tag_name"`
	HTMLURL string         `json:"html_url"`
	Assets  []releaseAsset `json:"assets"`
}

func defaultReleaseHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 60 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if err := validateReleaseURL(req.URL); err != nil {
				return err
			}
			return nil
		},
	}
}

func validateReleaseURL(u *url.URL) error {
	if u == nil || u.Scheme != "https" {
		return errors.New("release URL must use HTTPS")
	}
	host := strings.ToLower(u.Hostname())
	if !allowedReleaseHosts[host] {
		return errors.New("release URL host not allowed")
	}
	return nil
}

func fetchLatestRelease(ctx context.Context, client *http.Client, apiURL string, enforceHosts bool) (releaseInfo, error) {
	parsed, err := url.Parse(apiURL)
	if err != nil {
		return releaseInfo{}, err
	}
	if enforceHosts {
		if err := validateReleaseURL(parsed); err != nil {
			return releaseInfo{}, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return releaseInfo{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "souls-house-companion/"+version)

	resp, err := client.Do(req)
	if err != nil {
		return releaseInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return releaseInfo{}, fmt.Errorf("release server returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxReleaseMetaBytes+1))
	if err != nil {
		return releaseInfo{}, err
	}
	if len(body) > maxReleaseMetaBytes {
		return releaseInfo{}, errors.New("release metadata too large")
	}
	var info releaseInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return releaseInfo{}, err
	}
	if !isSafeVersion(info.TagName) {
		return releaseInfo{}, errors.New("release tag is missing or unsafe")
	}
	return info, nil
}

func (r releaseInfo) assetURL(name string) (string, bool) {
	for _, a := range r.Assets {
		if a.Name == name && a.BrowserDownloadURL != "" {
			return a.BrowserDownloadURL, true
		}
	}
	return "", false
}

func parseSHA256Sums(data []byte, filename string) (string, error) {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		sum := strings.ToLower(fields[0])
		name := strings.TrimPrefix(fields[1], "*")
		name = strings.TrimPrefix(name, "./")
		if name != filename {
			continue
		}
		if len(sum) != 64 {
			return "", errors.New("invalid SHA256SUMS digest length")
		}
		if _, err := hex.DecodeString(sum); err != nil {
			return "", errors.New("invalid SHA256SUMS digest")
		}
		return sum, nil
	}
	return "", fmt.Errorf("checksum for %s not found in SHA256SUMS", filename)
}

func downloadVerifiedFile(ctx context.Context, client *http.Client, rawURL, dstPath, wantSHA256 string, maxBytes int64, enforceHosts bool) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	if enforceHosts {
		if err := validateReleaseURL(parsed); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "souls-house-companion/"+version)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}

	f, err := os.OpenFile(dstPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxBytes+1))
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(dstPath)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(dstPath)
		return closeErr
	}
	if n > maxBytes {
		_ = os.Remove(dstPath)
		return errors.New("downloaded artifact exceeds size limit")
	}
	if wantSHA256 != "" {
		got := hex.EncodeToString(h.Sum(nil))
		if !strings.EqualFold(got, wantSHA256) {
			_ = os.Remove(dstPath)
			return newUserError("companion update: SHA-256 checksum mismatch; aborting without modifying installation", nil)
		}
	}
	return nil
}

// extractReleaseTarball extracts regular companion binaries from a verified .tar.gz
// into stageDir, refusing traversal, symlinks, hardlinks, and oversized files.
func extractReleaseTarball(tarPath, stageDir string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	allowedFiles := map[string]os.FileMode{
		"souls-house-companion": 0755,
		"souls-house-tray":      0755,
	}
	extracted := map[string]bool{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		clean := path.Clean(strings.TrimPrefix(hdr.Name, "./"))
		if clean == "." || clean == "" || strings.HasPrefix(clean, "/") || strings.HasPrefix(clean, "..") || strings.Contains(clean, "/../") {
			return errors.New("unsafe archive path")
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg, tar.TypeRegA:
			base := path.Base(clean)
			mode, ok := allowedFiles[base]
			if !ok {
				continue
			}
			if extracted[base] {
				return errors.New("duplicate binary in release archive")
			}
			if hdr.Size < 0 || hdr.Size > maxExtractedFileBytes {
				return errors.New("archive entry exceeds size limit")
			}
			dst := filepath.Join(stageDir, base)
			out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			n, copyErr := io.Copy(out, io.LimitReader(tr, maxExtractedFileBytes+1))
			syncErr := out.Sync()
			closeErr := out.Close()
			if copyErr != nil || syncErr != nil || closeErr != nil || n > maxExtractedFileBytes {
				_ = os.Remove(dst)
				return errors.New("failed to extract binary from archive")
			}
			extracted[base] = true
		case tar.TypeSymlink, tar.TypeLink:
			return errors.New("refusing symlink or hardlink in release archive")
		default:
			return errors.New("unsupported entry type in release archive")
		}
	}
	if !extracted["souls-house-companion"] {
		return errors.New("release archive missing souls-house-companion")
	}
	return nil
}
