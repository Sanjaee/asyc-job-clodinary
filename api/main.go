package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	_ "github.com/lib/pq"
	"github.com/streadway/amqp"
)

type Job struct {
	PostID    string   `json:"post_id"`
	Title     string   `json:"title"`
	Todo      string   `json:"todo"`
	FilePaths []string `json:"file_paths"`
	Timestamp string   `json:"timestamp"`
}

type Post struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Todo      string    `json:"todo"`
	ImageURL  string    `json:"imageurl"`
	CreatedAt time.Time `json:"created_at"`
}

type UploadResponse struct {
	Message   string   `json:"message"`
	PostID    string   `json:"post_id"`
	Status    string   `json:"status"` // "pending" or "processing"
	FilePaths []string `json:"file_paths"`
}

// Binary upload job types
type BinaryJob struct {
	JobID     string `json:"job_id"`
	FilePath  string `json:"file_path"`
	Timestamp string `json:"timestamp"`
}

type BinaryUploadResponse struct {
	Message string `json:"message"`
	JobID   string `json:"job_id"`
	Status  string `json:"status"` // "pending", "processing", "completed", "failed"
}

type BinaryUploadStatusResponse struct {
	JobID    string `json:"job_id"`
	Status   string `json:"status"` // "pending", "processing", "completed", "failed"
	ImageURL string `json:"image_url,omitempty"`
	Error    string `json:"error,omitempty"`
}

var (
	rabbitmqConn *amqp.Connection
	rabbitmqChan *amqp.Channel
	db           *sql.DB
)

func initDB() {
	var err error
	dbHost := os.Getenv("DB_HOST")
	if dbHost == "" {
		dbHost = "postgres"
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

	connStr := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		dbHost, dbPort, dbUser, dbPassword, dbName)

	db, err = sql.Open("postgres", connStr)
	if err != nil {
		log.Fatal("Failed to connect to database:", err)
	}

	// Wait for database to be ready
	maxRetries := 10
	for i := 0; i < maxRetries; i++ {
		err = db.Ping()
		if err == nil {
			break
		}
		log.Printf("Waiting for database... (attempt %d/%d)", i+1, maxRetries)
		time.Sleep(2 * time.Second)
	}

	if err != nil {
		log.Fatal("Failed to ping database:", err)
	}

	// Create table if not exists
	createTableSQL := `
	CREATE TABLE IF NOT EXISTS posts (
		id VARCHAR(255) PRIMARY KEY,
		title VARCHAR(255) NOT NULL,
		todo TEXT,
		imageurl TEXT,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);
	
	CREATE TABLE IF NOT EXISTS binary_uploads (
		job_id VARCHAR(255) PRIMARY KEY,
		status VARCHAR(50) NOT NULL DEFAULT 'pending',
		image_url TEXT,
		error_message TEXT,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);`

	_, err = db.Exec(createTableSQL)
	if err != nil {
		log.Fatal("Failed to create table:", err)
	}

	log.Println("Database connected and table created")
}

func initRabbitMQ() {
	var err error
	rabbitmqURL := os.Getenv("RABBITMQ_URL")
	if rabbitmqURL == "" {
		rabbitmqURL = "amqp://guest:guest@rabbitmq:5672/"
	}

	rabbitmqConn, err = amqp.Dial(rabbitmqURL)
	if err != nil {
		log.Fatal("Failed to connect to RabbitMQ:", err)
	}

	rabbitmqChan, err = rabbitmqConn.Channel()
	if err != nil {
		log.Fatal("Failed to open channel:", err)
	}

	// Declare queues
	_, err = rabbitmqChan.QueueDeclare(
		"image_processing",
		true,  // durable
		false, // delete when unused
		false, // exclusive
		false, // no-wait
		nil,   // arguments
	)
	if err != nil {
		log.Fatal("Failed to declare queue:", err)
	}

	_, err = rabbitmqChan.QueueDeclare(
		"binary_upload",
		true,  // durable
		false, // delete when unused
		false, // exclusive
		false, // no-wait
		nil,   // arguments
	)
	if err != nil {
		log.Fatal("Failed to declare binary_upload queue:", err)
	}

	log.Println("RabbitMQ connected and queues declared")
}

func publishJob(job Job) error {
	jobJSON, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("error marshaling job: %v", err)
	}

	err = rabbitmqChan.Publish(
		"",                 // exchange
		"image_processing", // routing key
		false,              // mandatory
		false,              // immediate
		amqp.Publishing{
			ContentType:  "application/json",
			Body:         jobJSON,
			DeliveryMode: amqp.Persistent, // Make message persistent
		},
	)
	if err != nil {
		return fmt.Errorf("error publishing to RabbitMQ: %v", err)
	}

	log.Printf("Job published to RabbitMQ: %s", job.PostID)
	return nil
}

func uploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse multipart form (max 50MB)
	err := r.ParseMultipartForm(50 << 20)
	if err != nil {
		http.Error(w, "Failed to parse form: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Get form fields
	title := r.FormValue("title")
	todo := r.FormValue("todo")

	if title == "" {
		http.Error(w, "Title is required", http.StatusBadRequest)
		return
	}

	// Get files (support both "images" and "files" field names)
	files := r.MultipartForm.File["images"]
	if len(files) == 0 {
		files = r.MultipartForm.File["files"]
	}
	if len(files) == 0 {
		http.Error(w, "No images provided. Use 'images' or 'files' field", http.StatusBadRequest)
		return
	}

	// Create tmp directory if not exists
	tmpDir := "/tmp"
	if _, err := os.Stat(tmpDir); os.IsNotExist(err) {
		os.MkdirAll(tmpDir, os.ModePerm)
	}

	postID := uuid.New().String()
	var filePaths []string

	// Save each file temporarily
	for _, fileHeader := range files {
		file, err := fileHeader.Open()
		if err != nil {
			log.Printf("Error opening file: %v", err)
			continue
		}

		// Generate unique filename
		ext := filepath.Ext(fileHeader.Filename)
		if ext == "" {
			ext = ".jpg"
		}
		filename := uuid.New().String() + ext
		filePath := filepath.Join(tmpDir, filename)

		// Create file
		dst, err := os.Create(filePath)
		if err != nil {
			file.Close()
			log.Printf("Error creating file: %v", err)
			continue
		}

		// Copy file content
		_, err = io.Copy(dst, file)
		file.Close()
		dst.Close()

		if err != nil {
			log.Printf("Error saving file: %v", err)
			os.Remove(filePath) // Clean up on error
			continue
		}

		filePaths = append(filePaths, filePath)
		log.Printf("File saved temporarily: %s (size: %d bytes)", filePath, fileHeader.Size)
	}

	if len(filePaths) == 0 {
		http.Error(w, "No valid files processed", http.StatusBadRequest)
		return
	}

	// Insert post with PENDING status (imageurl will be empty initially)
	insertSQL := `INSERT INTO posts (id, title, todo, imageurl) VALUES ($1, $2, $3, $4)
		ON CONFLICT (id) DO UPDATE SET title = $2, todo = $3`
	_, err = db.Exec(insertSQL, postID, title, todo, "")
	if err != nil {
		log.Printf("Error saving to database: %v", err)
		// Clean up files on error
		for _, path := range filePaths {
			os.Remove(path)
		}
		http.Error(w, "Failed to save post: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Create job for worker
	job := Job{
		PostID:    postID,
		Title:     title,
		Todo:      todo,
		FilePaths: filePaths,
		Timestamp: time.Now().Format(time.RFC3339),
	}

	// Publish to RabbitMQ (async processing)
	err = publishJob(job)
	if err != nil {
		log.Printf("Error publishing job: %v", err)
		// Clean up files on error
		for _, path := range filePaths {
			os.Remove(path)
		}
		http.Error(w, "Failed to queue job: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Return response immediately (ASYNC - worker will process in background)
	response := UploadResponse{
		Message:   "Files uploaded and queued for processing",
		PostID:    postID,
		Status:    "pending",
		FilePaths: filePaths,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(response)
}

func getPostsHandler(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query("SELECT id, title, todo, imageurl, created_at FROM posts ORDER BY created_at DESC")
	if err != nil {
		http.Error(w, "Failed to query database: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var posts []Post
	for rows.Next() {
		var post Post
		err := rows.Scan(&post.ID, &post.Title, &post.Todo, &post.ImageURL, &post.CreatedAt)
		if err != nil {
			log.Printf("Error scanning row: %v", err)
			continue
		}
		posts = append(posts, post)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(posts)
}

func getPostHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	postID := vars["id"]

	var post Post
	err := db.QueryRow("SELECT id, title, todo, imageurl, created_at FROM posts WHERE id = $1", postID).
		Scan(&post.ID, &post.Title, &post.Todo, &post.ImageURL, &post.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "Post not found", http.StatusNotFound)
			return
		}
		http.Error(w, "Failed to query database: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(post)
}

func uploadBinaryHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Read binary data from request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	if len(body) == 0 {
		http.Error(w, "No file data provided", http.StatusBadRequest)
		return
	}

	// Generate job ID
	jobID := uuid.New().String()

	// Create tmp directory if not exists
	tmpDir := "/tmp"
	if _, err := os.Stat(tmpDir); os.IsNotExist(err) {
		os.MkdirAll(tmpDir, os.ModePerm)
	}

	// Determine file extension from Content-Type or default to .jpg
	contentType := r.Header.Get("Content-Type")
	ext := ".jpg"
	if contentType != "" {
		if strings.Contains(contentType, "png") {
			ext = ".png"
		} else if strings.Contains(contentType, "jpeg") || strings.Contains(contentType, "jpg") {
			ext = ".jpg"
		} else if strings.Contains(contentType, "webp") {
			ext = ".webp"
		}
	}

	// Save file temporarily
	filename := jobID + ext
	filePath := filepath.Join(tmpDir, filename)

	err = os.WriteFile(filePath, body, 0644)
	if err != nil {
		http.Error(w, "Failed to save file: "+err.Error(), http.StatusInternalServerError)
		return
	}

	log.Printf("Binary file saved: %s (size: %d bytes)", filePath, len(body))

	// Insert job status to database
	insertSQL := `INSERT INTO binary_uploads (job_id, status) VALUES ($1, $2)`
	_, err = db.Exec(insertSQL, jobID, "pending")
	if err != nil {
		log.Printf("Error saving job to database: %v", err)
		os.Remove(filePath) // Clean up on error
		http.Error(w, "Failed to save job: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Create binary job
	binaryJob := BinaryJob{
		JobID:     jobID,
		FilePath:  filePath,
		Timestamp: time.Now().Format(time.RFC3339),
	}

	// Publish to RabbitMQ
	jobJSON, err := json.Marshal(binaryJob)
	if err != nil {
		log.Printf("Error marshaling job: %v", err)
		os.Remove(filePath)
		http.Error(w, "Failed to create job: "+err.Error(), http.StatusInternalServerError)
		return
	}

	err = rabbitmqChan.Publish(
		"",              // exchange
		"binary_upload", // routing key
		false,           // mandatory
		false,           // immediate
		amqp.Publishing{
			ContentType:  "application/json",
			Body:         jobJSON,
			DeliveryMode: amqp.Persistent,
		},
	)
	if err != nil {
		log.Printf("Error publishing job: %v", err)
		os.Remove(filePath)
		http.Error(w, "Failed to queue job: "+err.Error(), http.StatusInternalServerError)
		return
	}

	log.Printf("Binary upload job published: %s", jobID)

	// Return response
	response := BinaryUploadResponse{
		Message: "File uploaded and queued for processing",
		JobID:   jobID,
		Status:  "pending",
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(response)
}

func binaryUploadStatusHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	jobID := vars["job_id"]

	var status string
	var imageURL sql.NullString
	var errorMsg sql.NullString

	err := db.QueryRow(
		"SELECT status, image_url, error_message FROM binary_uploads WHERE job_id = $1",
		jobID,
	).Scan(&status, &imageURL, &errorMsg)

	if err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "Job not found", http.StatusNotFound)
			return
		}
		http.Error(w, "Failed to query database: "+err.Error(), http.StatusInternalServerError)
		return
	}

	response := BinaryUploadStatusResponse{
		JobID:  jobID,
		Status: status,
	}

	if imageURL.Valid {
		response.ImageURL = imageURL.String
	}

	if errorMsg.Valid {
		response.Error = errorMsg.String
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func main() {
	// Initialize database
	initDB()
	defer db.Close()

	// Initialize RabbitMQ
	initRabbitMQ()
	defer rabbitmqConn.Close()
	defer rabbitmqChan.Close()

	// Setup router
	r := mux.NewRouter()

	// Routes
	r.HandleFunc("/api/upload", uploadHandler).Methods("POST")
	r.HandleFunc("/api/upload-binary", uploadBinaryHandler).Methods("POST")
	r.HandleFunc("/api/upload-status/{job_id}", binaryUploadStatusHandler).Methods("GET")
	r.HandleFunc("/api/posts", getPostsHandler).Methods("GET")
	r.HandleFunc("/api/posts/{id}", getPostHandler).Methods("GET")
	r.HandleFunc("/health", healthHandler).Methods("GET")

	// CORS middleware
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusOK)
				return
			}
			next.ServeHTTP(w, r)
		})
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("API Server starting on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, r))
}
