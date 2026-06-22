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
	maxBucketBytes = 9 * 1024 * 1024 * 1024 // 9 GB лімит
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
	http.HandleFunc("/delete", handleDelete)

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

	headers := r.MultipartForm.File["photo"]
	if len(headers) == 0 {
		http.Error(w, "Не передано жодного файлу", http.StatusBadRequest)
		return
	}

	//check backet limit
	var totalNewBytes int64
	for _, h := range headers {
		totalNewBytes += h.Size
	}

	currentSize, err := getBucketTotalSize(r.Context())
	if err != nil {
		log.Printf("Помилка підрахунку розміру бакета: %v", err)
		http.Error(w, fmt.Sprintf("Помилка перевірки розміру бакета: %v", err), http.StatusInternalServerError)
		return
	}

	if currentSize+totalNewBytes > maxBucketBytes {
		msg := fmt.Sprintf(
			"Відмова: бакет заповнений (%.2f GB з %.2f GB лімітом), %d файл(и) (%.2f MB разом) не влізуть",
			float64(currentSize)/(1024*1024*1024),
			float64(maxBucketBytes)/(1024*1024*1024),
			len(headers),
			float64(totalNewBytes)/(1024*1024),
		)
		log.Println("" + msg)
		http.Error(w, msg, http.StatusInsufficientStorage) // 507
		return
	}

	// --- Завантажуємо кожен файл по черзі ---
	var uploaded []string
	var failed []string

	for _, header := range headers {
		file, err := header.Open()
		if err != nil {
			log.Printf("Помилка відкриття файлу %s: %v", header.Filename, err)
			failed = append(failed, header.Filename)
			continue
		}

		fmt.Printf("Отримано файл: %s, розмір: %d байт\n", header.Filename, header.Size)

		_, err = s3Client.PutObject(context.TODO(), &s3.PutObjectInput{
			Bucket:      aws.String(bucketName),
			Key:         aws.String(header.Filename),
			Body:        file,
			ContentType: aws.String(header.Header.Get("Content-Type")),
		})
		file.Close()

		if err != nil {
			log.Printf("Помилка завантаження %s в R2: %v", header.Filename, err)
			failed = append(failed, header.Filename)
			continue
		}

		uploaded = append(uploaded, header.Filename)
	}

	if len(failed) > 0 {
		w.WriteHeader(http.StatusMultiStatus) // 207 some photos are loaded
		fmt.Fprintf(w, "Завантажено: %d (%v). Помилка з: %v", len(uploaded), uploaded, failed)
		return
	}

	fmt.Fprintf(w, "Успішно завантажено %d файл(и): %v", len(uploaded), uploaded)
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

func handleDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Тільки DELETE", http.StatusMethodNotAllowed)
		return
	}

	key := r.URL.Query().Get("key")
	if key == "" {
		http.Error(w, "Не вказано параметр key", http.StatusBadRequest)
		return
	}

	_, err := s3Client.DeleteObject(r.Context(), &s3.DeleteObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(key),
	})
	if err != nil {
		log.Printf("Помилка видалення %s: %v", key, err)
		http.Error(w, fmt.Sprintf("Помилка видалення: %v", err), http.StatusInternalServerError)
		return
	}

	fmt.Printf("Видалено файл: %s\n", key)
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Файл %s видалено", key)
}