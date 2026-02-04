package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
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

type Post struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Todo      string    `json:"todo"`
	ImageURL  string    `json:"imageurl"`
	CreatedAt time.Time `json:"created_at"`
}

type ImageUploadResponse struct {
	Message   string   `json:"message"`
	PostID    string   `json:"post_id"`
	ImageURLs []string `json:"image_urls"`
	Status    string   `json:"status"`
}

var (
	db          *sql.DB
	rabbitmqConn *amqp.Connection
	rabbitmqChan *amqp.Channel
)

func initDB() {
	var err error
	connStr := "host=localhost port=5432 user=postgres password=postgres dbname=clodinary_db sslmode=disable"
	db, err = sql.Open("postgres", connStr)
	if err != nil {
		log.Fatal("Failed to connect to database:", err)
	}

	// Create table if not exists
	createTableSQL := `
	CREATE TABLE IF NOT EXISTS posts (
		id VARCHAR(255) PRIMARY KEY,
		title VARCHAR(255) NOT NULL,
		todo TEXT,
		imageurl TEXT,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);`

	_, err = db.Exec(createTableSQL)
	if err != nil {
		log.Fatal("Failed to create table:", err)
	}

	log.Println("Database connected and table created")
}

func initRabbitMQ() {
	var err error
	connStr := "amqp://guest:guest@localhost:5672/"
	rabbitmqConn, err = amqp.Dial(connStr)
	if err != nil {
		log.Fatal("Failed to connect to RabbitMQ:", err)
	}

	rabbitmqChan, err = rabbitmqConn.Channel()
	if err != nil {
		log.Fatal("Failed to open channel:", err)
	}

	// Declare queue
	_, err = rabbitmqChan.QueueDeclare(
		"image_processing", // queue name
		true,               // durable
		false,              // delete when unused
		false,              // exclusive
		false,              // no-wait
		nil,                // arguments
	)
	if err != nil {
		log.Fatal("Failed to declare queue:", err)
	}

	log.Println("RabbitMQ connected and queue declared")
}

func compressImage(imageData []byte, format string) ([]byte, error) {
	var img image.Image
	var err error

	reader := bytes.NewReader(imageData)

	if format == "image/jpeg" || format == "image/jpg" {
		img, err = jpeg.Decode(reader)
		if err != nil {
			return nil, err
		}
	} else if format == "image/png" {
		img, err = png.Decode(reader)
		if err != nil {
			return nil, err
		}
	} else {
		return nil, fmt.Errorf("unsupported image format: %s", format)
	}

	// Create compressed buffer
	var buf bytes.Buffer
	writer := io.Writer(&buf)

	// Compress as JPEG with quality 70
	if err := jpeg.Encode(writer, img, &jpeg.Options{Quality: 70}); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func uploadImageHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse multipart form
	err := r.ParseMultipartForm(32 << 20) // 32 MB max
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

	// Get files
	files := r.MultipartForm.File["images"]
	if len(files) == 0 {
		http.Error(w, "No images provided", http.StatusBadRequest)
		return
	}

	// Create uploads directory if not exists
	uploadDir := "./uploads"
	os.MkdirAll(uploadDir, os.ModePerm)

	postID := uuid.New().String()
	var imageURLs []string
	var compressedImages [][]byte

	// Process each image
	for _, fileHeader := range files {
		file, err := fileHeader.Open()
		if err != nil {
			log.Printf("Error opening file: %v", err)
			continue
		}
		defer file.Close()

		// Read file data
		imageData := make([]byte, fileHeader.Size)
		_, err = file.Read(imageData)
		if err != nil {
			log.Printf("Error reading file: %v", err)
			continue
		}

		// Detect content type
		contentType := http.DetectContentType(imageData)
		if !strings.HasPrefix(contentType, "image/") {
			log.Printf("File is not an image: %s", contentType)
			continue
		}

		// Compress image
		compressedData, err := compressImage(imageData, contentType)
		if err != nil {
			log.Printf("Error compressing image: %v", err)
			continue
		}

		// Generate unique filename
		ext := filepath.Ext(fileHeader.Filename)
		if ext == "" {
			ext = ".jpg"
		}
		filename := uuid.New().String() + ext
		filePath := filepath.Join(uploadDir, filename)

		// Save compressed image
		err = os.WriteFile(filePath, compressedData, 0644)
		if err != nil {
			log.Printf("Error saving file: %v", err)
			continue
		}

		imageURL := fmt.Sprintf("/uploads/%s", filename)
		imageURLs = append(imageURLs, imageURL)
		compressedImages = append(compressedImages, compressedData)

		log.Printf("Image saved: %s (original: %d bytes, compressed: %d bytes)", 
			filename, len(imageData), len(compressedData))
	}

	if len(imageURLs) == 0 {
		http.Error(w, "No valid images processed", http.StatusBadRequest)
		return
	}

	// Combine all image URLs into one string
	imageURLsStr := strings.Join(imageURLs, ",")

	// Save to database
	insertSQL := `INSERT INTO posts (id, title, todo, imageurl) VALUES ($1, $2, $3, $4)`
	_, err = db.Exec(insertSQL, postID, title, todo, imageURLsStr)
	if err != nil {
		log.Printf("Error saving to database: %v", err)
		http.Error(w, "Failed to save to database: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Send to RabbitMQ
	message := map[string]interface{}{
		"post_id":    postID,
		"title":      title,
		"todo":       todo,
		"image_urls": imageURLs,
		"timestamp":  time.Now().Format(time.RFC3339),
	}

	messageJSON, err := json.Marshal(message)
	if err != nil {
		log.Printf("Error marshaling message: %v", err)
	} else {
		err = rabbitmqChan.Publish(
			"",                // exchange
			"image_processing", // routing key
			false,             // mandatory
			false,             // immediate
			amqp.Publishing{
				ContentType: "application/json",
				Body:        messageJSON,
			},
		)
		if err != nil {
			log.Printf("Error publishing to RabbitMQ: %v", err)
		} else {
			log.Printf("Message sent to RabbitMQ: %s", string(messageJSON))
		}
	}

	// Return response
	response := ImageUploadResponse{
		Message:   "Images uploaded and processed successfully",
		PostID:    postID,
		ImageURLs: imageURLs,
		Status:    "completed",
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
	r.HandleFunc("/api/upload", uploadImageHandler).Methods("POST")
	r.HandleFunc("/api/posts", getPostsHandler).Methods("GET")
	r.HandleFunc("/api/posts/{id}", getPostHandler).Methods("GET")

	// Serve uploaded files
	r.PathPrefix("/uploads/").Handler(http.StripPrefix("/uploads/", http.FileServer(http.Dir("./uploads"))))

	// Health check
	r.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}).Methods("GET")

	log.Println("Server starting on :8080")
	log.Fatal(http.ListenAndServe(":8080", r))
}
