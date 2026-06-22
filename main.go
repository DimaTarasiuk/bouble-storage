package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/joho/godotenv"
)

const (
	maxBucketBytes = 9 * 1024 * 1024 * 1024 // 9 GB limit
	pageSize       = 10
)

var (
	s3Client      *s3.Client
	presignClient *s3.PresignClient
	bucketName    string
)

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("Не задана змінна середовища %s (перевір .env або Render Environment)", key)
	}
	return v
}

func main() {
	_ = godotenv.Load()

	r2AccountID := mustEnv("R2_ACCOUNT_ID")
	r2AccessKey := mustEnv("R2_ACCESS_KEY_ID")
	r2SecretKey := mustEnv("R2_SECRET_ACCESS_KEY")
	bucketName = mustEnv("R2_BUCKET_NAME")

	r2Endpoint := fmt.Sprintf("https://%s.r2.cloudflarestorage.com", r2AccountID)

	cfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(r2AccessKey, r2SecretKey, "")),
		config.WithRegion("auto"),
	)
	if err != nil {
		log.Fatalf("Помилка конфігурації AWS SDK: %v", err)
	}

	s3Client = s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(r2Endpoint)
	})
	presignClient = s3.NewPresignClient(s3Client)

	//checkR2Access()

	http.HandleFunc("/", serveHTML)
	http.HandleFunc("/upload", handleUpload)
	http.HandleFunc("/list", handleList)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
 
	addr := "0.0.0.0:" + port
	fmt.Printf("Сервер запущено на http://%s\n", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

func checkR2Access() {
	_, err := s3Client.HeadBucket(context.TODO(), &s3.HeadBucketInput{
		Bucket: aws.String(bucketName),
	})
	if err != nil {
		log.Fatalf("Немає доступу до бакета %q: %v", bucketName, err)
	}
	fmt.Printf("Доступ до R2 бакета %q успішний\n", bucketName)
}

func serveHTML(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "index.html")
}

func getBucketTotalSize(ctx context.Context) (int64, error) {
	var total int64
	var continuationToken *string

	for {
		out, err := s3Client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(bucketName),
			ContinuationToken: continuationToken,
		})
		if err != nil {
			return 0, err
		}

		for _, obj := range out.Contents {
			total += aws.ToInt64(obj.Size)
		}

		if out.IsTruncated != nil && *out.IsTruncated {
			continuationToken = out.NextContinuationToken
			continue
		}
		break
	}

	return total, nil
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Тільки POST", http.StatusMethodNotAllowed)
		return
	}

	// Обмеження розміру одного файлу (10MB)
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20)

	err := r.ParseMultipartForm(10 << 20)
	if err != nil {
		http.Error(w, fmt.Sprintf("Помилка парсингу форми: %v", err), http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("photo")
	if err != nil {
		http.Error(w, fmt.Sprintf("Помилка отримання файлу: %v", err), http.StatusBadRequest)
		return
	}
	defer file.Close()

	// bucket check
	currentSize, err := getBucketTotalSize(r.Context())
	if err != nil {
		log.Printf("Помилка підрахунку розміру бакета: %v", err)
		http.Error(w, fmt.Sprintf("Помилка перевірки розміру бакета: %v", err), http.StatusInternalServerError)
		return
	}

	if currentSize+header.Size > maxBucketBytes {
		msg := fmt.Sprintf(
			"Відмова: бакет заповнений (%.2f GB з %.2f GB лімітом), новий файл (%.2f MB) не влізе",
			float64(currentSize)/(1024*1024*1024),
			float64(maxBucketBytes)/(1024*1024*1024),
			float64(header.Size)/(1024*1024),
		)
		log.Println("" + msg)
		http.Error(w, msg, http.StatusInsufficientStorage) // 507
		return
	}

	fmt.Printf("Отримано файл: %s, розмір: %d байт (бакет зараз: %.2f GB)\n",
		header.Filename, header.Size, float64(currentSize)/(1024*1024*1024))

	_, err = s3Client.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket:      aws.String(bucketName),
		Key:         aws.String(header.Filename),
		Body:        file,
		ContentType: aws.String(header.Header.Get("Content-Type")),
	})
	if err != nil {
		log.Printf("Помилка завантаження в R2: %v", err)
		http.Error(w, fmt.Sprintf("Помилка завантаження в R2: %v", err), http.StatusInternalServerError)
		return
	}

	fmt.Fprintf(w, "Файл %s успішно завантажено в бакет %s", header.Filename, bucketName)
}

// --- Структури для JSON відповіді /list ---

type ImageItem struct {
	Key          string `json:"key"`
	SizeBytes    int64  `json:"sizeBytes"`
	LastModified string `json:"lastModified"`
	URL          string `json:"url"`
}

type ListResponse struct {
	Items      []ImageItem `json:"items"`
	Page       int         `json:"page"`
	TotalPages int         `json:"totalPages"`
	TotalItems int         `json:"totalItems"`
}

func handleList(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}

	var allObjects []types.Object
	var continuationToken *string

	for {
		out, err := s3Client.ListObjectsV2(r.Context(), &s3.ListObjectsV2Input{
			Bucket:            aws.String(bucketName),
			ContinuationToken: continuationToken,
		})
		if err != nil {
			http.Error(w, fmt.Sprintf("Помилка отримання списку: %v", err), http.StatusInternalServerError)
			return
		}
		allObjects = append(allObjects, out.Contents...)

		if out.IsTruncated != nil && *out.IsTruncated {
			continuationToken = out.NextContinuationToken
			continue
		}
		break
	}

	// sort new first
	sort.Slice(allObjects, func(i, j int) bool {
		return allObjects[i].LastModified.After(*allObjects[j].LastModified)
	})

	totalItems := len(allObjects)
	totalPages := (totalItems + pageSize - 1) / pageSize
	if totalPages == 0 {
		totalPages = 1
	}

	start := (page - 1) * pageSize
	end := start + pageSize
	if start > totalItems {
		start = totalItems
	}
	if end > totalItems {
		end = totalItems
	}

	pageObjects := allObjects[start:end]
	items := make([]ImageItem, 0, len(pageObjects))

	for _, obj := range pageObjects {
		presignedReq, err := presignClient.PresignGetObject(r.Context(), &s3.GetObjectInput{
			Bucket: aws.String(bucketName),
			Key:    obj.Key,
		}, s3.WithPresignExpires(15*time.Minute))
		if err != nil {
			log.Printf("Помилка генерації presigned URL для %s: %v", *obj.Key, err)
			continue
		}

		items = append(items, ImageItem{
			Key:          aws.ToString(obj.Key),
			SizeBytes:    aws.ToInt64(obj.Size),
			LastModified: obj.LastModified.Format(time.RFC3339),
			URL:          presignedReq.URL,
		})
	}

	resp := ListResponse{
		Items:      items,
		Page:       page,
		TotalPages: totalPages,
		TotalItems: totalItems,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}