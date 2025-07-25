package archivers

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"gorm.io/gorm"
)

// YTArchiver (streams directly from yt-dlp stdout)
type YTArchiver struct{}

func (a *YTArchiver) Archive(url string, logWriter io.Writer, db *gorm.DB, itemID uint) (io.Reader, string, string, func(), error) {
	fmt.Fprintf(logWriter, "Starting YouTube archive for: %s\n", url)
	
	// First, test if yt-dlp can access the video
	fmt.Fprintf(logWriter, "Testing video accessibility with yt-dlp...\n")
	testCmd := exec.Command("yt-dlp", "--print", "title,duration,uploader", url)
	testOutput, err := testCmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(logWriter, "yt-dlp test failed: %v\nOutput: %s\n", err, string(testOutput))
		return nil, "", "", nil, fmt.Errorf("yt-dlp cannot access video: %v", err)
	}
	fmt.Fprintf(logWriter, "Video info:\n%s\n", string(testOutput))
	
	cmd := exec.Command("yt-dlp", "-f", "bestvideo+bestaudio/best", "--no-playlist", "--no-write-thumbnail", "--verbose", "-o", "-", url)
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
	
	// Start capturing stderr in a goroutine
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := stderrPipe.Read(buf)
			if n > 0 {
				logWriter.Write(buf[:n])
				// Also write to stdout for immediate debugging
				os.Stdout.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
	}()
	
	fmt.Fprintf(logWriter, "Starting yt-dlp download process...\n")
	if err = cmd.Start(); err != nil {
		fmt.Fprintf(logWriter, "Failed to start yt-dlp: %v\n", err)
		return nil, "", "", nil, err
	}
	cleanup := func() { 
		cmd.Process.Kill()
		cmd.Wait() 
	}
	fmt.Fprintf(logWriter, "YouTube download started successfully\n")
	return pr, ".mp4", "video/mp4", cleanup, nil
}
