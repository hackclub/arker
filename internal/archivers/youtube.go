package archivers

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"gorm.io/gorm"
)

// YTArchiver (streams directly from yt-dlp stdout)
type YTArchiver struct{}

func (a *YTArchiver) Archive(ctx context.Context, url string, logWriter io.Writer, db *gorm.DB, itemID uint) (io.Reader, string, string, *PWBundle, error) {
	fmt.Fprintf(logWriter, "Starting YouTube archive for: %s\n", url)
	
	// Check context before starting
	select {
	case <-ctx.Done():
		return nil, "", "", nil, ctx.Err()
	default:
	}
	
	// Prepare command arguments
	testArgs := []string{"--print", "title,duration,uploader"}
	dlArgs := []string{"-f", "bestvideo+bestaudio/best", "--no-playlist", "--no-write-thumbnail", "--verbose", "-o", "-"}
	
	// Add SOCKS5 proxy configuration if SOCKS5_PROXY is set
	if socks5Proxy := os.Getenv("SOCKS5_PROXY"); socks5Proxy != "" {
		fmt.Fprintf(logWriter, "Using SOCKS5 proxy for yt-dlp: %s\n", socks5Proxy)
		testArgs = append([]string{"--proxy", socks5Proxy}, testArgs...)
		dlArgs = append([]string{"--proxy", socks5Proxy}, dlArgs...)
	}
	
	// First, test if yt-dlp can access the video
	fmt.Fprintf(logWriter, "Testing video accessibility with yt-dlp...\n")
	testCmd := exec.CommandContext(ctx, "yt-dlp")
	testCmd.Args = append(testCmd.Args, testArgs...)
	testCmd.Args = append(testCmd.Args, url)
	testOutput, err := testCmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(logWriter, "yt-dlp test failed: %v\nOutput: %s\n", err, string(testOutput))
		return nil, "", "", nil, fmt.Errorf("yt-dlp cannot access video: %v", err)
	}
	fmt.Fprintf(logWriter, "Video info:\n%s\n", string(testOutput))
	
	// Check context before main download
	select {
	case <-ctx.Done():
		return nil, "", "", nil, ctx.Err()
	default:
	}
	
	cmd := exec.CommandContext(ctx, "yt-dlp")
	cmd.Args = append(cmd.Args, dlArgs...)
	cmd.Args = append(cmd.Args, url)
	
	pr, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to create stdout pipe: %v\n", err)
		return nil, "", "", nil, err
	}
	
	// Create a pipe for stderr so we can capture and forward logs
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to create stderr pipe: %v\n", err)
		return nil, "", "", nil, err
	}
	
	// Start context-aware stderr capturing in a goroutine
	go func() {
		defer stderrPipe.Close()
		buf := make([]byte, 1024)
		
		for {
			// Check context before each read
			select {
			case <-ctx.Done():
				fmt.Fprintf(logWriter, "Context cancelled during yt-dlp stderr capture\n")
				return
			default:
			}
			
			// Use a timeout for the read operation to avoid blocking indefinitely
			type readResult struct {
				n   int
				err error
			}
			
			readChan := make(chan readResult, 1)
			go func() {
				n, err := stderrPipe.Read(buf)
				readChan <- readResult{n: n, err: err}
			}()
			
			// Wait for either read completion or context cancellation
			select {
			case <-ctx.Done():
				fmt.Fprintf(logWriter, "Context cancelled during yt-dlp stderr capture\n")
				return
			case result := <-readChan:
				if result.n > 0 {
					logWriter.Write(buf[:result.n])
				}
				if result.err != nil {
					if result.err != io.EOF {
						fmt.Fprintf(logWriter, "yt-dlp stderr read error: %v\n", result.err)
					}
					return
				}
			}
		}
	}()
	
	fmt.Fprintf(logWriter, "Starting yt-dlp download process...\n")
	if err = cmd.Start(); err != nil {
		fmt.Fprintf(logWriter, "Failed to start yt-dlp: %v\n", err)
		return nil, "", "", nil, err
	}
	
	fmt.Fprintf(logWriter, "YouTube download started successfully\n")
	return pr, ".mp4", "video/mp4", nil, nil
}
