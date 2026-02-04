package main

import (
	"context"
	"database/sql/driver"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudinary/cloudinary-go/v2"
	"github.com/cloudinary/cloudinary-go/v2/api/uploader"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// StringArray type for PostgreSQL array
type StringArray []string

func (a StringArray) Value() (driver.Value, error) {
	if len(a) == 0 {
		return "{}", nil
	}
	return "{" + strings.Join(a, ",") + "}", nil
}

func (a *StringArray) Scan(value interface{}) error {
	if value == nil {
		*a = StringArray{}
		return nil
	}

	var str string
	switch v := value.(type) {
	case []byte:
		str = string(v)
	case string:
		str = v
	default:
		return fmt.Errorf("cannot scan %T into StringArray", value)
	}

	// Remove { } from PostgreSQL array format
	str = strings.Trim(str, "{}")
	if str == "" {
		*a = StringArray{}
		return nil
	}

	*a = StringArray(strings.Split(str, ","))
	return nil
}

// Database Models
type Post struct {
	ID        string      `gorm:"primaryKey" json:"id"`
	Title     string      `gorm:"not null" json:"title"`
	Todo      string      `json:"todo"`
	ImageURL  StringArray `gorm:"type:text[]" json:"image_url"`
	Status    string      `gorm:"default:pending" json:"status"` // pending, processing, completed, failed
	CreatedAt time.Time   `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt time.Time   `gorm:"autoUpdateTime" json:"updated_at"`
}

type BinaryUpload struct {
	JobID        string    `gorm:"primaryKey" json:"job_id"`
	Status       string    `gorm:"default:pending" json:"status"` // pending, processing, completed, failed
	ImageURL     string    `json:"image_url"`
	ErrorMessage string    `json:"error_message,omitempty"`
	CreatedAt    time.Time `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt    time.Time `gorm:"autoUpdateTime" json:"updated_at"`
}

// Global variables
var (
	db  *gorm.DB
	cld *cloudinary.Cloudinary
)

// Initialize database
func initDB() {
	dbHost := os.Getenv("DB_HOST")
	if dbHost == "" {
		dbHost = "localhost"
	}
	dbPort := os.Getenv("DB_PORT")
	if dbPort == "" {
		dbPort = "5432"
	}
	dbUser := os.Getenv("DB_USER")
	if dbUser == "" {
		dbUser = "postgres"
	}
	dbPassword := os.Getenv("DB_PASSWORD")
	if dbPassword == "" {
		dbPassword = "postgres"
	}
	dbName := os.Getenv("DB_NAME")
	if dbName == "" {
		dbName = "clodinary_db"
	}

	dsn := fmt.Sprintf("host=%s user=%s password=%s dbname=%s port=%s sslmode=disable TimeZone=Asia/Jakarta",
		dbHost, dbUser, dbPassword, dbName, dbPort)

	var err error
	db, err = gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatal("Failed to connect to database:", err)
	}

	// Auto migrate
	err = db.AutoMigrate(&Post{}, &BinaryUpload{})
	if err != nil {
		log.Fatal("Failed to migrate database:", err)
	}

	log.Println("Database connected and migrated")
}

// Initialize Cloudinary
func initCloudinary() {
	cloudName := os.Getenv("CLOUDINARY_CLOUD_NAME")
	apiKey := os.Getenv("CLOUDINARY_API_KEY")
	apiSecret := os.Getenv("CLOUDINARY_API_SECRET")

	if cloudName == "" || apiKey == "" || apiSecret == "" {
		log.Fatal("Cloudinary credentials not found. Please set CLOUDINARY_CLOUD_NAME, CLOUDINARY_API_KEY, and CLOUDINARY_API_SECRET")
	}

	var err error
	cld, err = cloudinary.NewFromParams(cloudName, apiKey, apiSecret)
	if err != nil {
		log.Fatal("Cloudinary init error:", err)
	}

	log.Println("Cloudinary initialized successfully")
}

// Compress image
func compressImage(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("error opening file: %v", err)
	}
	defer file.Close()

	var img image.Image
	ext := strings.ToLower(filepath.Ext(filePath))

	if ext == ".jpg" || ext == ".jpeg" {
		img, err = jpeg.Decode(file)
		if err != nil {
			return "", fmt.Errorf("error decoding JPEG: %v", err)
		}
	} else if ext == ".png" {
		img, err = png.Decode(file)
		if err != nil {
			return "", fmt.Errorf("error decoding PNG: %v", err)
		}
	} else if ext == ".webp" {
		// For WebP, upload directly without compression
		return filePath, nil
	} else {
		return "", fmt.Errorf("unsupported image format: %s", ext)
	}

	compressedPath := filePath + ".compressed.jpg"
	compressedFile, err := os.Create(compressedPath)
	if err != nil {
		return "", fmt.Errorf("error creating compressed file: %v", err)
	}
	defer compressedFile.Close()

	err = jpeg.Encode(compressedFile, img, &jpeg.Options{Quality: 80})
	if err != nil {
		return "", fmt.Errorf("error encoding compressed image: %v", err)
	}

	return compressedPath, nil
}

// Upload to Cloudinary
func uploadToCloudinary(filePath string) (string, error) {
	ctx := context.Background()

	result, err := cld.Upload.Upload(ctx, filePath, uploader.UploadParams{
		Folder:         "uploads",
		Transformation: "q_auto,f_auto,w_1280",
		ResourceType:   "image",
	})

	if err != nil {
		return "", fmt.Errorf("error uploading to Cloudinary: %v", err)
	}

	return result.SecureURL, nil
}

// Process file from memory
func processFile(fileData []byte, filename string) (string, error) {
	// Create temporary file
	tmpDir := "/tmp"
	ext := filepath.Ext(filename)
	if ext == "" {
		ext = ".jpg"
	}
	tempFile := filepath.Join(tmpDir, uuid.New().String()+ext)

	err := os.WriteFile(tempFile, fileData, 0644)
	if err != nil {
		return "", fmt.Errorf("error writing temp file: %v", err)
	}
	defer os.Remove(tempFile) // Clean up temp file

	// Compress
	compressedPath, err := compressImage(tempFile)
	if err != nil {
		log.Printf("Error compressing, using original: %v", err)
		compressedPath = tempFile
	} else {
		if compressedPath != tempFile {
			defer os.Remove(compressedPath) // Clean up compressed file
		}
	}

	// Upload to Cloudinary
	imageURL, err := uploadToCloudinary(compressedPath)
	if err != nil {
		return "", err
	}

	return imageURL, nil
}

// Request/Response DTOs
type UploadRequest struct {
	Title string          `form:"title" binding:"required"`
	Todo  string          `form:"todo"`
	Files []*UploadedFile `form:"images" binding:"required"`
}

type UploadedFile struct {
	Filename string
	Data     []byte
	MimeType string
}

type UploadResponse struct {
	Message string `json:"message"`
	PostID  string `json:"post_id"`
	Status  string `json:"status"`
}

type BinaryUploadResponse struct {
	Message string `json:"message"`
	JobID   string `json:"job_id"`
	Status  string `json:"status"`
}

type BinaryUploadStatusResponse struct {
	JobID    string `json:"job_id"`
	Status   string `json:"status"`
	ImageURL string `json:"image_url,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Handlers
func uploadHandler(c *gin.Context) {
	var req struct {
		Title string `form:"title" binding:"required"`
		Todo  string `form:"todo"`
	}

	if err := c.ShouldBind(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	form, err := c.MultipartForm()
	if err != nil {
		c.JSON(400, gin.H{"error": "Failed to parse form"})
		return
	}

	files := form.File["images"]
	if len(files) == 0 {
		files = form.File["files"]
	}
	if len(files) == 0 {
		c.JSON(400, gin.H{"error": "No images provided"})
		return
	}

	postID := uuid.New().String()

	// Read all files into memory
	type FileData struct {
		Data     []byte
		Filename string
	}
	var fileDataList []FileData

	for _, fileHeader := range files {
		file, err := fileHeader.Open()
		if err != nil {
			log.Printf("Error opening file: %v", err)
			continue
		}

		data, err := io.ReadAll(file)
		file.Close()
		if err != nil {
			log.Printf("Error reading file: %v", err)
			continue
		}

		fileDataList = append(fileDataList, FileData{
			Data:     data,
			Filename: fileHeader.Filename,
		})
	}

	if len(fileDataList) == 0 {
		c.JSON(400, gin.H{"error": "No valid files processed"})
		return
	}

	// Create post with pending status (no image_url yet)
	post := Post{
		ID:       postID,
		Title:    req.Title,
		Todo:     req.Todo,
		ImageURL: StringArray{}, // Will be updated after processing
		Status:   "pending",
	}

	if err := db.Create(&post).Error; err != nil {
		c.JSON(500, gin.H{"error": "Failed to save post"})
		return
	}

	// Process files in background (async)
	go func() {
		// Update status to processing
		db.Model(&post).Update("status", "processing")

		var imageURLs []string

		// Process each file
		for _, fileData := range fileDataList {
			// Process file (compress & upload)
			imageURL, err := processFile(fileData.Data, fileData.Filename)
			if err != nil {
				log.Printf("Error processing file %s: %v", fileData.Filename, err)
				continue
			}

			imageURLs = append(imageURLs, imageURL)
			log.Printf("Uploaded to Cloudinary: %s", imageURL)
		}

		if len(imageURLs) == 0 {
			// Update status to failed
			db.Model(&post).Update("status", "failed")
			log.Printf("No images were successfully processed for post %s", postID)
			return
		}

		// Update post with image URLs (as array) and status
		db.Model(&post).Updates(map[string]interface{}{
			"image_url": StringArray(imageURLs),
			"status":    "completed",
		})

		log.Printf("Post %s processing completed with %d images", postID, len(imageURLs))
	}()

	// Return response immediately (ASYNC)
	c.JSON(200, UploadResponse{
		Message: "Files uploaded and queued for processing",
		PostID:  postID,
		Status:  "pending",
	})
}

func uploadBinaryHandler(c *gin.Context) {
	// Read binary data
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(400, gin.H{"error": "Failed to read request body"})
		return
	}
	defer c.Request.Body.Close()

	if len(body) == 0 {
		c.JSON(400, gin.H{"error": "No file data provided"})
		return
	}

	jobID := uuid.New().String()

	// Create binary upload record
	binaryUpload := BinaryUpload{
		JobID:  jobID,
		Status: "processing",
	}

	if err := db.Create(&binaryUpload).Error; err != nil {
		c.JSON(500, gin.H{"error": "Failed to create job"})
		return
	}

	// Process file in background (goroutine)
	go func() {
		// Update status to processing
		db.Model(&binaryUpload).Update("status", "processing")

		// Process file
		imageURL, err := processFile(body, "upload.jpg")
		if err != nil {
			// Update with error
			db.Model(&binaryUpload).Updates(map[string]interface{}{
				"status":        "failed",
				"error_message": err.Error(),
			})
			return
		}

		// Update with success
		db.Model(&binaryUpload).Updates(map[string]interface{}{
			"status":    "completed",
			"image_url": imageURL,
		})

		log.Printf("Binary upload job completed: %s -> %s", jobID, imageURL)
	}()

	c.JSON(200, BinaryUploadResponse{
		Message: "File uploaded and queued for processing",
		JobID:   jobID,
		Status:  "processing",
	})
}

func binaryUploadStatusHandler(c *gin.Context) {
	jobID := c.Param("job_id")

	var binaryUpload BinaryUpload
	if err := db.First(&binaryUpload, "job_id = ?", jobID).Error; err != nil {
		c.JSON(404, gin.H{"error": "Job not found"})
		return
	}

	response := BinaryUploadStatusResponse{
		JobID:  binaryUpload.JobID,
		Status: binaryUpload.Status,
	}

	if binaryUpload.ImageURL != "" {
		response.ImageURL = binaryUpload.ImageURL
	}

	if binaryUpload.ErrorMessage != "" {
		response.Error = binaryUpload.ErrorMessage
	}

	c.JSON(200, response)
}

func getPostsHandler(c *gin.Context) {
	var posts []Post
	if err := db.Order("created_at DESC").Find(&posts).Error; err != nil {
		c.JSON(500, gin.H{"error": "Failed to query posts"})
		return
	}

	c.JSON(200, posts)
}

func getPostHandler(c *gin.Context) {
	id := c.Param("id")

	var post Post
	if err := db.First(&post, "id = ?", id).Error; err != nil {
		c.JSON(404, gin.H{"error": "Post not found"})
		return
	}

	c.JSON(200, post)
}

func healthHandler(c *gin.Context) {
	c.String(200, "OK")
}

func main() {
	// Initialize
	initDB()
	initCloudinary()

	// Setup Gin
	r := gin.Default()

	// CORS middleware
	r.Use(func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}

		c.Next()
	})

	// Routes
	r.GET("/health", healthHandler)
	r.POST("/api/upload", uploadHandler)
	r.POST("/api/upload-binary", uploadBinaryHandler)
	r.GET("/api/upload-status/:job_id", binaryUploadStatusHandler)
	r.GET("/api/posts", getPostsHandler)
	r.GET("/api/posts/:id", getPostHandler)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("Server starting on :%s", port)
	log.Fatal(r.Run(":" + port))
}
