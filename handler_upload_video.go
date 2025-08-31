package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	// 1. Set upload limit to 1 GB
	const maxUploadSize = 1 << 30 // 1 GB
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)

	// 2. Extract and parse videoID from URL
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid video ID", err)
		return
	}

	// 3. Authenticate the user
	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}
	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	// 4. Get video metadata and check ownership
	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusNotFound, "Video not found", err)
		return
	}
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "You are not authorized to upload this video", nil)
		return
	}

	// 5. Parse the uploaded video file from form data
	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't get video file from form", err)
		return
	}
	defer file.Close()

	// 6. Validate the uploaded file is a video/mp4
	contentType := header.Header.Get("Content-Type")
	parsedMediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Failed to parse media type", err)
		return
	}
	if parsedMediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, fmt.Sprintf("Unsupported file type: %s. Only MP4 videos are allowed.", parsedMediaType), nil)
		return
	}

	// 7. Save the uploaded file to a temporary file on disk
	tempFile, err := os.CreateTemp("", "tubely-upload-*.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create temp file", err)
		return
	}
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	// 8. Copy contents over
	if _, err := io.Copy(tempFile, file); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't copy video to temp file", err)
		return
	}

	// 9. Reset the temp file's pointer to the beginning for processing and S3 upload
	if _, err := tempFile.Seek(0, io.SeekStart); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't reset temp file pointer", err)
		return
	}

	// 10. Process the video for fast start
	processedFilePath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't process video for fast start", err)
		return
	}
	defer os.Remove(processedFilePath)

	// 11. Get aspect ratio and determine S3 key prefix
	aspectRatio, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get video aspect ratio", err)
		return
	}

	var s3KeyPrefix string
	switch aspectRatio {
	case "16:9":
		s3KeyPrefix = "landscape"
	case "9:16":
		s3KeyPrefix = "portrait"
	default:
		s3KeyPrefix = "other"
	}

	// 12. Put the processed video into S3
	randBytes := make([]byte, 32)
	if _, err := rand.Read(randBytes); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not generate random filename for S3 key", err)
		return
	}
	s3Key := s3KeyPrefix + "/" + base64.RawURLEncoding.EncodeToString(randBytes) + ".mp4"

	processedFile, err := os.Open(processedFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't open processed video file", err)
		return
	}
	defer processedFile.Close()

	putObjectInput := &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &s3Key,
		Body:        processedFile,
		ContentType: &contentType,
	}

	if _, err := cfg.s3Client.PutObject(r.Context(), putObjectInput); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't upload file to S3", err)
		return
	}

	// 13. Update the video record in the database with the S3 URL
	videoURL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, s3Key)
	video.VideoURL = &videoURL
	if err := cfg.db.UpdateVideo(video); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video record", err)
		return
	}

	// 14. Respond with the updated JSON
	respondWithJSON(w, http.StatusOK, video)
}

// getVideoAspectRatio uses ffprobe to determine the video's aspect ratio.
func getVideoAspectRatio(filePath string) (string, error) {
	// A simple struct to unmarshal the relevant parts of the ffprobe output
	type ProbeStream struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	}
	type ProbeOutput struct {
		Streams []ProbeStream `json:"streams"`
	}

	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-print_format", "json",
		"-show_streams",
		filePath,
	)

	var out bytes.Buffer
	cmd.Stdout = &out

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("could not run ffprobe: %w", err)
	}

	var probeOutput ProbeOutput
	if err := json.Unmarshal(out.Bytes(), &probeOutput); err != nil {
		return "", fmt.Errorf("could not unmarshal ffprobe output: %w", err)
	}

	if len(probeOutput.Streams) == 0 {
		return "other", nil
	}

	width := float64(probeOutput.Streams[0].Width)
	height := float64(probeOutput.Streams[0].Height)

	if height == 0 {
		return "other", nil
	}

	ratio := width / height

	// Check for a landscape (16:9) aspect ratio with a small tolerance
	if ratio > 1.7 && ratio < 1.8 {
		return "16:9", nil
	}

	// Check for a portrait (9:16) aspect ratio with a small tolerance
	if ratio > 0.55 && ratio < 0.57 {
		return "9:16", nil
	}

	return "other", nil
}

// processVideoForFastStart creates a new video file with "fast start" encoding.
func processVideoForFastStart(filePath string) (string, error) {
	processedFilePath := filePath + ".processing"

	cmd := exec.Command("ffmpeg",
		"-i", filePath,
		"-c", "copy",
		"-movflags", "faststart",
		"-f", "mp4",
		processedFilePath,
	)

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("could not run ffmpeg: %w", err)
	}

	return processedFilePath, nil
}
