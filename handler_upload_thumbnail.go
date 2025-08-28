package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

// getFileExtension determines the correct file extension from a Content-Type header.
func getFileExtension(contentType string) (string, error) {
	switch contentType {
	case "image/jpeg":
		return ".jpg", nil
	case "image/png":
		return ".png", nil
	case "image/gif":
		return ".gif", nil
	default:
		return "", fmt.Errorf("unsupported content type: %s", contentType)
	}
}

func (cfg *apiConfig) handlerUploadThumbnail(w http.ResponseWriter, r *http.Request) {
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

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

	fmt.Println("uploading thumbnail for video", videoID, "by user", userID)

	// 1. Parse the form data
	const maxMemory = 10 << 20 // 10 MB
	err = r.ParseMultipartForm(maxMemory)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Failed to parse form data", err)
		return
	}

	// 2. Get the image data from the form
	file, header, err := r.FormFile("thumbnail")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't get thumbnail file from form", err)
		return
	}
	defer file.Close()

	// 3. Get the media type from the file's Content-Type header
	mediaType := header.Header.Get("Content-Type")
	if mediaType == "" {
		respondWithError(w, http.StatusBadRequest, "Content-Type header is missing", nil)
		return
	}

	// Determine the file extension from the Content-Type
	fileExt, err := getFileExtension(mediaType)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, err.Error(), nil)
		return
	}

	// 4. Create a unique file path on disk
	filename := fmt.Sprintf("%s%s", videoID.String(), fileExt)
	filePath := filepath.Join(cfg.assetsRoot, filename)

	// 5. Create the new file on the filesystem
	dst, err := os.Create(filePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create file on disk", err)
		return
	}
	defer dst.Close()

	// 6. Copy the contents from the form file to the new file on disk
	_, err = io.Copy(dst, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't save file to disk", err)
		return
	}

	// 7. Get the video's metadata from the database
	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusNotFound, "Video not found", err)
		return
	}

	// Check if the authenticated user is the video owner
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "You are not authorized to upload a thumbnail for this video", nil)
		return
	}

	// 8. Update the video metadata with the new thumbnail URL
	thumbnailURL := fmt.Sprintf("http://localhost:%s/assets/%s", cfg.port, filename)
	video.ThumbnailURL = &thumbnailURL // Pass a pointer to the string

	// 9. Update the record in the database
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video metadata", err)
		return
	}

	// 10. Respond with the updated JSON
	respondWithJSON(w, http.StatusOK, video)
}
