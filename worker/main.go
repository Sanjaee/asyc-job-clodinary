package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudinary/cloudinary-go/v2"
	"github.com/cloudinary/cloudinary-go/v2/api/uploader"
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

type BinaryJob struct {
	JobID     string `json:"job_id"`
	FilePath  string `json:"file_path"`
	Timestamp string `json:"timestamp"`
}

var (
	db  *sql.DB
	cld *cloudinary.Cloudinary
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

func compressImage(filePath string) (string, error) {
	// Read file
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("error opening file: %v", err)
	}
	defer file.Close()

	// Detect image format
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
	} else {
		return "", fmt.Errorf("unsupported image format: %s", ext)
	}

	// Create compressed file
	compressedPath := filePath + ".compressed.jpg"
	compressedFile, err := os.Create(compressedPath)
	if err != nil {
		return "", fmt.Errorf("error creating compressed file: %v", err)
	}
	defer compressedFile.Close()

	// Compress as JPEG with quality 80
	err = jpeg.Encode(compressedFile, img, &jpeg.Options{Quality: 80})
	if err != nil {
		return "", fmt.Errorf("error encoding compressed image: %v", err)
	}

	return compressedPath, nil
}

func uploadToCloudinary(filePath string) (string, error) {
	ctx := context.Background()

	// Upload with auto optimization
	result, err := cld.Upload.Upload(ctx, filePath, uploader.UploadParams{
		Folder:         "uploads",
		Transformation: "q_auto,f_auto,w_1280", // Auto quality, auto format (WebP/AVIF), max width 1280
		ResourceType:   "image",
	})

	if err != nil {
		return "", fmt.Errorf("error uploading to Cloudinary: %v", err)
	}

	return result.SecureURL, nil
}

func processJob(job Job) error {
	log.Printf("Processing job: %s (Title: %s)", job.PostID, job.Title)

	var imageURLs []string

	// Process each file
	for _, filePath := range job.FilePaths {
		// Check if file exists
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			log.Printf("File not found: %s", filePath)
			continue
		}

		// Compress image
		compressedPath, err := compressImage(filePath)
		if err != nil {
			log.Printf("Error compressing image %s: %v", filePath, err)
			// Try to upload original if compression fails
			compressedPath = filePath
		} else {
			log.Printf("Image compressed: %s -> %s", filePath, compressedPath)
		}

		// Upload to Cloudinary
		imageURL, err := uploadToCloudinary(compressedPath)
		if err != nil {
			log.Printf("Error uploading to Cloudinary: %v", err)
			// Clean up compressed file if it was created
			if compressedPath != filePath {
				os.Remove(compressedPath)
			}
			continue
		}

		imageURLs = append(imageURLs, imageURL)
		log.Printf("Uploaded to Cloudinary: %s", imageURL)

		// Clean up files (unless KEEP_FILES is set for debugging)
		keepFiles := os.Getenv("KEEP_FILES")
		if keepFiles != "true" && keepFiles != "1" {
			os.Remove(filePath)
			if compressedPath != filePath {
				os.Remove(compressedPath)
			}
		} else {
			log.Printf("Keeping files for debugging: %s, %s", filePath, compressedPath)
		}
	}

	if len(imageURLs) == 0 {
		return fmt.Errorf("no images were successfully processed")
	}

	// Combine image URLs
	imageURLsStr := strings.Join(imageURLs, ",")

	// Save to database
	insertSQL := `INSERT INTO posts (id, title, todo, imageurl) VALUES ($1, $2, $3, $4)
		ON CONFLICT (id) DO UPDATE SET imageurl = $4`
	_, err := db.Exec(insertSQL, job.PostID, job.Title, job.Todo, imageURLsStr)
	if err != nil {
		return fmt.Errorf("error saving to database: %v", err)
	}

	log.Printf("Job completed successfully: %s", job.PostID)
	return nil
}

func processBinaryJob(binaryJob BinaryJob) error {
	log.Printf("Processing binary upload job: %s", binaryJob.JobID)

	// Update status to processing
	updateSQL := `UPDATE binary_uploads SET status = 'processing', updated_at = CURRENT_TIMESTAMP WHERE job_id = $1`
	_, err := db.Exec(updateSQL, binaryJob.JobID)
	if err != nil {
		log.Printf("Error updating job status: %v", err)
	}

	// Check if file exists
	if _, err := os.Stat(binaryJob.FilePath); os.IsNotExist(err) {
		errorMsg := fmt.Sprintf("File not found: %s", binaryJob.FilePath)
		updateErrorSQL := `UPDATE binary_uploads SET status = 'failed', error_message = $1, updated_at = CURRENT_TIMESTAMP WHERE job_id = $2`
		db.Exec(updateErrorSQL, errorMsg, binaryJob.JobID)
		return fmt.Errorf(errorMsg)
	}

	// Compress image
	compressedPath, err := compressImage(binaryJob.FilePath)
	if err != nil {
		log.Printf("Error compressing image %s: %v", binaryJob.FilePath, err)
		// Try to upload original if compression fails
		compressedPath = binaryJob.FilePath
	} else {
		log.Printf("Image compressed: %s -> %s", binaryJob.FilePath, compressedPath)
	}

	// Upload to Cloudinary
	imageURL, err := uploadToCloudinary(compressedPath)
	if err != nil {
		errorMsg := fmt.Sprintf("Error uploading to Cloudinary: %v", err)
		log.Printf(errorMsg)
		updateErrorSQL := `UPDATE binary_uploads SET status = 'failed', error_message = $1, updated_at = CURRENT_TIMESTAMP WHERE job_id = $2`
		db.Exec(updateErrorSQL, errorMsg, binaryJob.JobID)

		// Clean up compressed file if it was created
		if compressedPath != binaryJob.FilePath {
			os.Remove(compressedPath)
		}
		return fmt.Errorf(errorMsg)
	}

	log.Printf("Uploaded to Cloudinary: %s", imageURL)

	// Update database with success
	updateSuccessSQL := `UPDATE binary_uploads SET status = 'completed', image_url = $1, updated_at = CURRENT_TIMESTAMP WHERE job_id = $2`
	_, err = db.Exec(updateSuccessSQL, imageURL, binaryJob.JobID)
	if err != nil {
		log.Printf("Error updating job with success: %v", err)
	}

	// Clean up files (unless KEEP_FILES is set)
	keepFiles := os.Getenv("KEEP_FILES")
	if keepFiles != "true" && keepFiles != "1" {
		os.Remove(binaryJob.FilePath)
		if compressedPath != binaryJob.FilePath {
			os.Remove(compressedPath)
		}
	} else {
		log.Printf("Keeping files for debugging: %s, %s", binaryJob.FilePath, compressedPath)
	}

	log.Printf("Binary upload job completed successfully: %s", binaryJob.JobID)
	return nil
}

func main() {
	// Initialize database
	initDB()
	defer db.Close()

	// Initialize Cloudinary
	initCloudinary()

	// Connect to RabbitMQ
	rabbitmqURL := os.Getenv("RABBITMQ_URL")
	if rabbitmqURL == "" {
		rabbitmqURL = "amqp://guest:guest@rabbitmq:5672/"
	}

	conn, err := amqp.Dial(rabbitmqURL)
	if err != nil {
		log.Fatal("Failed to connect to RabbitMQ:", err)
	}
	defer conn.Close()

	ch, err := conn.Channel()
	if err != nil {
		log.Fatal("Failed to open channel:", err)
	}
	defer ch.Close()

	// Declare queues
	q, err := ch.QueueDeclare(
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

	binaryQ, err := ch.QueueDeclare(
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

	// Set QoS to process one message at a time
	err = ch.Qos(
		1,     // prefetch count
		0,     // prefetch size
		false, // global
	)
	if err != nil {
		log.Fatal("Failed to set QoS:", err)
	}

	// Consume messages
	msgs, err := ch.Consume(
		q.Name, // queue
		"",     // consumer
		false,  // auto-ack (set to false for manual ack)
		false,  // exclusive
		false,  // no-local
		false,  // no-wait
		nil,    // args
	)
	if err != nil {
		log.Fatal("Failed to register consumer:", err)
	}

	// Consume binary upload queue
	binaryMsgs, err := ch.Consume(
		binaryQ.Name, // queue
		"",           // consumer
		false,        // auto-ack (set to false for manual ack)
		false,        // exclusive
		false,        // no-local
		false,        // no-wait
		nil,          // args
	)
	if err != nil {
		log.Fatal("Failed to register binary upload consumer:", err)
	}

	log.Println("Worker started. Waiting for messages...")

	// Process messages from both queues using goroutines
	go func() {
		for msg := range msgs {
			var job Job
			err := json.Unmarshal(msg.Body, &job)
			if err != nil {
				log.Printf("Error decoding message: %v", err)
				msg.Nack(false, false) // Reject and don't requeue
				continue
			}

			// Process job
			err = processJob(job)
			if err != nil {
				log.Printf("Error processing job: %v", err)
				msg.Nack(false, true) // Reject and requeue
				continue
			}

			// Acknowledge message
			msg.Ack(false)
			log.Printf("Job processed and acknowledged: %s", job.PostID)
		}
	}()

	// Process binary upload messages
	for msg := range binaryMsgs {
		var binaryJob BinaryJob
		err := json.Unmarshal(msg.Body, &binaryJob)
		if err != nil {
			log.Printf("Error decoding binary job message: %v", err)
			msg.Nack(false, false) // Reject and don't requeue
			continue
		}

		// Process binary job
		err = processBinaryJob(binaryJob)
		if err != nil {
			log.Printf("Error processing binary job: %v", err)
			msg.Nack(false, true) // Reject and requeue
			continue
		}

		// Acknowledge message
		msg.Ack(false)
		log.Printf("Binary upload job processed and acknowledged: %s", binaryJob.JobID)
	}
}
