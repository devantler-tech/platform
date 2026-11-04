package acrapplicability

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"golang.org/x/mod/sumdb/dirhash"
)

// Let Go resolve its own immutable download, then verify the actual extracted
// source bytes rather than trusting cached version metadata or a build ID alone.
func verifyModule(ctx context.Context, work, goBin, path, version, want string) (string, error) {
	data, _, _, err := run(ctx, work, cleanEnvironment(false), time.Minute, goBin, "mod", "download", "-json", path+"@"+version)
	if err != nil {
		return "", err
	}
	var download struct {
		Path, Version, Dir, Sum string
		Error                   any
	}
	if err := json.Unmarshal(data, &download); err != nil || download.Error != nil || download.Path != path || download.Version != version || download.Sum != want || download.Dir == "" {
		return "", fmt.Errorf("native module download identity or checksum mismatch")
	}
	got, err := dirhash.HashDir(download.Dir, path+"@"+version, dirhash.Hash1)
	if err != nil || got != want {
		return "", fmt.Errorf("cached module source differs from its verified checksum: %s", path)
	}
	return got, nil
}
