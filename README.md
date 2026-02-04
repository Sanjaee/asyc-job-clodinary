# Clodinary - Image Upload & Processing dengan Cloudinary + RabbitMQ

Project Go untuk upload multi image, compress, dan upload ke Cloudinary menggunakan RabbitMQ sebagai message queue.

## 🏗️ Struktur Project

```
clodinary/
├── api/
│   ├── main.go          # API server untuk upload
│   ├── Dockerfile
│   └── go.mod
├── worker/
│   ├── main.go          # Worker untuk process & upload ke Cloudinary
│   ├── Dockerfile
│   └── go.mod
├── docker-compose.yml   # Docker setup
├── .env                 # Environment variables (JANGAN DI COMMIT!)
└── README.md
```

## 🚀 Quick Start

### 1. Setup Environment Variables

Copy `.env.example` ke `.env` dan isi dengan credentials Cloudinary:

```bash
cp .env.example .env
```

Edit `.env`:
```env
CLOUDINARY_CLOUD_NAME=your_cloud_name
CLOUDINARY_API_KEY=your_api_key
CLOUDINARY_API_SECRET=your_api_secret
```

### 2. Run dengan Docker Compose

```bash
docker compose up --build
```

Services akan running di:
- **API**: http://localhost:8080
- **RabbitMQ Management UI**: http://localhost:15672 (guest/guest)
- **PostgreSQL**: localhost:5432

### 3. Test Upload dengan Postman

**Endpoint**: `POST http://localhost:8080/api/upload`

**Body** (form-data):
- `title`: "My Post Title" (required)
- `todo`: "My todo description" (optional)
- `images`: [file1.jpg, file2.png, ...] (required, multiple files)

**Response**:
```json
{
  "message": "Files uploaded and queued for processing",
  "post_id": "uuid-here",
  "status": "queued",
  "file_paths": ["/tmp/..."]
}
```

### 4. Lihat Hasil

**Get All Posts**:
```
GET http://localhost:8080/api/posts
```

**Get Post by ID**:
```
GET http://localhost:8080/api/posts/{post_id}
```

## 📋 API Endpoints

| Method | Endpoint | Description |
|--------|----------|-------------|
| POST | `/api/upload` | Upload multi image (form-data) |
| GET | `/api/posts` | Get all posts |
| GET | `/api/posts/{id}` | Get post by ID |
| GET | `/health` | Health check |

## 🔄 Flow

1. **Upload** → API menerima file, simpan ke `/tmp`, kirim job ke RabbitMQ
2. **Worker** → Consume dari RabbitMQ, compress image, upload ke Cloudinary
3. **Database** → Worker simpan hasil (title, todo, imageurl) ke PostgreSQL
4. **Result** → Lihat hasil via GET `/api/posts`

## 🐳 Docker Services

- **postgres**: PostgreSQL database
- **rabbitmq**: RabbitMQ message broker
- **api**: API server (port 8080)
- **worker**: Background worker untuk process images

## 🔐 Security

- ✅ Cloudinary credentials hanya di Worker (tidak di API)
- ✅ `.env` file tidak di-commit (ada di `.gitignore`)
- ✅ API tidak perlu tahu Cloudinary secrets

## 📦 Dependencies

### API
- `github.com/gorilla/mux` - HTTP router
- `github.com/streadway/amqp` - RabbitMQ client
- `github.com/lib/pq` - PostgreSQL driver
- `github.com/google/uuid` - UUID generator

### Worker
- `github.com/cloudinary/cloudinary-go/v2` - Cloudinary SDK
- `github.com/lib/pq` - PostgreSQL driver
- `github.com/streadway/amqp` - RabbitMQ client

## 🧪 Testing dengan Postman

1. Buka Postman
2. Create new request: `POST http://localhost:8080/api/upload`
3. Body → form-data:
   - `title`: Text → "Test Post"
   - `todo`: Text → "Test Todo"
   - `images`: File → Select multiple images
4. Send request
5. Check response untuk `post_id`
6. GET `/api/posts/{post_id}` untuk lihat hasil setelah worker selesai

## 📊 Database Schema

```sql
CREATE TABLE posts (
    id VARCHAR(255) PRIMARY KEY,
    title VARCHAR(255) NOT NULL,
    todo TEXT,
    imageurl TEXT,  -- Comma-separated URLs
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
```

## 🔍 Monitoring

- **RabbitMQ UI**: http://localhost:15672
  - Username: `guest`
  - Password: `guest`
  - Check queue `image_processing` untuk melihat jobs

## 🐛 Troubleshooting

### Worker tidak process jobs
- Check logs: `docker compose logs worker`
- Pastikan `.env` file ada dan credentials benar
- Check RabbitMQ connection di logs

### Database connection error
- Pastikan PostgreSQL sudah running: `docker compose ps`
- Check connection string di logs

### Cloudinary upload error
- Pastikan credentials di `.env` benar
- Check Cloudinary dashboard untuk quota/limits

## 📝 Notes

- Images di-compress dengan quality 80 sebelum upload
- Cloudinary auto-optimize (WebP/AVIF) dengan max width 1280px
- File temporary di `/tmp` akan dihapus setelah processing
- Multiple images akan di-join dengan comma di `imageurl` field
