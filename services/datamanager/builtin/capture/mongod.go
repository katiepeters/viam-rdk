package capture

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/utils"
)

const (
	mongodVersion      = "7.0.14"
	pipelineMongodPort = 27018
)

func mongodBinPath() string {
	return filepath.Join(utils.ViamDotDir, "bin", "mongod")
}

func mongodDataDir() string {
	return filepath.Join(utils.ViamDotDir, "mongod-data")
}

func mongodDownloadURL() (string, error) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64":
		return fmt.Sprintf("https://fastdl.mongodb.org/linux/mongodb-linux-x86_64-%s.tgz", mongodVersion), nil
	case "linux/arm64":
		return fmt.Sprintf("https://fastdl.mongodb.org/linux/mongodb-linux-aarch64-%s.tgz", mongodVersion), nil
	case "darwin/amd64":
		return fmt.Sprintf("https://fastdl.mongodb.org/osx/mongodb-macos-x86_64-%s.tgz", mongodVersion), nil
	case "darwin/arm64":
		return fmt.Sprintf("https://fastdl.mongodb.org/osx/mongodb-macos-arm64-%s.tgz", mongodVersion), nil
	default:
		return "", fmt.Errorf("unsupported platform for mongod: %s/%s", runtime.GOOS, runtime.GOARCH)
	}
}

// ensureMongodBinary downloads the mongod binary to destPath if it is not already present.
// The binary is extracted from the official MongoDB Community tarball for the current platform.
func ensureMongodBinary(ctx context.Context, destPath string, logger logging.Logger) error {
	if _, err := os.Stat(destPath); err == nil {
		return nil
	}
	url, err := mongodDownloadURL()
	if err != nil {
		return err
	}
	logger.Infof("pipeline: downloading mongod %s", mongodVersion)
	if err := os.MkdirAll(filepath.Dir(destPath), 0o700); err != nil {
		return fmt.Errorf("create bin dir: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download mongod: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download mongod: HTTP %d", resp.StatusCode)
	}
	return extractMongodBinary(resp.Body, destPath)
}

func extractMongodBinary(r io.Reader, destPath string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close() //nolint:errcheck
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("mongod binary not found in archive")
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}
		if !strings.HasSuffix(hdr.Name, "/bin/mongod") {
			continue
		}
		tmp := destPath + ".tmp"
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o700)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			os.Remove(tmp) //nolint:errcheck
			return fmt.Errorf("write mongod binary: %w", err)
		}
		f.Close()
		return os.Rename(tmp, destPath)
	}
}

// mongodProcess manages a mongod child process started by viam-server for pipeline execution.
// The binary lives at ~/.viam/bin/mongod so viam knows it owns it.
type mongodProcess struct {
	cmd    *exec.Cmd
	logger logging.Logger
}

// launchMongod starts a mongod process and waits for it to accept connections.
// Returns the process handle and a connected *mongo.Client.
func launchMongod(ctx context.Context, binPath, dataDir string, logger logging.Logger) (*mongodProcess, *mongo.Client, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create mongod data dir: %w", err)
	}
	logPath := filepath.Join(utils.ViamDotDir, "mongod.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open mongod log: %w", err)
	}
	cmd := exec.Command(binPath,
		"--dbpath", dataDir,
		"--port", fmt.Sprintf("%d", pipelineMongodPort),
		"--bind_ip", "127.0.0.1",
		"--noauth",
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close() //nolint:errcheck
		return nil, nil, fmt.Errorf("start mongod: %w", err)
	}
	logFile.Close() //nolint:errcheck // child inherited the fd

	proc := &mongodProcess{cmd: cmd, logger: logger}
	client, err := proc.waitReady(ctx)
	if err != nil {
		proc.stop()
		return nil, nil, fmt.Errorf("mongod ready: %w", err)
	}
	logger.Infof("pipeline: mongod ready on port %d", pipelineMongodPort)
	return proc, client, nil
}

func (p *mongodProcess) waitReady(ctx context.Context) (*mongo.Client, error) {
	uri := fmt.Sprintf("mongodb://127.0.0.1:%d/?directConnection=true", pipelineMongodPort)
	client, err := mongo.Connect(ctx, options.Client().
		ApplyURI(uri).
		SetServerSelectionTimeout(30*time.Second))
	if err != nil {
		return nil, err
	}
	if err := client.Ping(ctx, nil); err != nil {
		client.Disconnect(ctx) //nolint:errcheck
		return nil, err
	}
	return client, nil
}

func (p *mongodProcess) stop() {
	if p.cmd == nil || p.cmd.Process == nil {
		return
	}
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		p.logger.Warnf("pipeline: SIGTERM mongod: %v", err)
		_ = p.cmd.Process.Kill()
	}
	done := make(chan struct{})
	go func() {
		_ = p.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		p.logger.Warn("pipeline: mongod did not exit in 5s, killing")
		_ = p.cmd.Process.Kill()
		<-done
	}
}
