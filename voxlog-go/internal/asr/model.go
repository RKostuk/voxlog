package asr

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type ModelSpec struct {
	Family  string
	Variant string
	Files   []ModelFile

	// SupportsLanguage is true for models that accept an explicit language
	// hint. Whisper/Parakeet auto-detect and have no pinning API, so the
	// Settings window forces those to "auto" rather than offering a picker
	// that would silently do nothing.
	SupportsLanguage bool

	// SupportsStreaming is true for models with a live partial-transcript
	// path (the OnlineRecognizer engines). Gates the "live streaming text"
	// option, which is meaningless for the batch engines.
	SupportsStreaming bool

	// Description is the one-line trade-off summary shown beside the model
	// in Settings -- speed vs accuracy vs download size, so the choice can
	// be made without looking anything up.
	Description string
}

type ModelFile struct {
	URL      string
	Filename string
}

func ModelDir(baseDir string, m ModelSpec) string {
	return filepath.Join(baseDir, fmt.Sprintf("%s-%s", m.Family, m.Variant))
}

func IsDownloaded(baseDir string, m ModelSpec) bool {
	dir := ModelDir(baseDir, m)
	for _, f := range m.Files {
		if !fileExists(dir, f.Filename) {
			return false
		}
	}
	return len(m.Files) > 0
}

// fileExists reports whether dir/filename exists and is non-empty.
func fileExists(dir, filename string) bool {
	info, err := os.Stat(filepath.Join(dir, filename))
	return err == nil && info.Size() > 0
}

type ProgressFunc func(file string, downloaded, total int64)
type FetchFunc func(url string) (io.ReadCloser, int64, error)

func Download(baseDir string, m ModelSpec, progress ProgressFunc, fetch FetchFunc) error {
	dir := ModelDir(baseDir, m)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	for _, f := range m.Files {
		dest := filepath.Join(dir, f.Filename)
		if info, err := os.Stat(dest); err == nil && info.Size() > 0 {
			continue
		}

		body, total, err := fetch(f.URL)
		if err != nil {
			return fmt.Errorf("fetch %s: %w", f.URL, err)
		}

		tmp := dest + ".part"
		out, err := os.Create(tmp)
		if err != nil {
			body.Close()
			return err
		}

		var downloaded int64
		buf := make([]byte, 32*1024)
		for {
			n, rerr := body.Read(buf)
			if n > 0 {
				if _, werr := out.Write(buf[:n]); werr != nil {
					body.Close()
					out.Close()
					os.Remove(tmp)
					return werr
				}
				downloaded += int64(n)
				progress(f.Filename, downloaded, total)
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				body.Close()
				out.Close()
				os.Remove(tmp)
				return rerr
			}
		}
		body.Close()
		if err := out.Close(); err != nil {
			os.Remove(tmp)
			return err
		}
		if err := os.Rename(tmp, dest); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	return nil
}
