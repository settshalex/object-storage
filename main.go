package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/gorilla/mux"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"hash/crc32"
	"io"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
)

// MinioClientInterface defines the methods needed from the Minio client.
type MinioClientInterface interface {
	GetObject(ctx context.Context, bucketName, objectName string, opts minio.GetObjectOptions) (*minio.Object, error)
	MakeBucket(ctx context.Context, bucketName string, opts minio.MakeBucketOptions) error
	BucketExists(ctx context.Context, bucketName string) (bool, error)
	PutObject(ctx context.Context, bucketName string, objectName string, reader io.Reader, objectSize int64, opts minio.PutObjectOptions) (info minio.UploadInfo, err error)
}

// A MinioClients slice to store all clients active
type MinioClients struct {
	minioClients []MinioClientInterface
}

// responsePutObject result of put objects operation
type responsePutObject struct {
	Bucket string `json:"bucket"`
	Size   int64  `json:"size"`
	Sha256 string `json:"sha256"`
}

// Main function to start server
func main() {
	// Set logging level
	loggingLevel := zerolog.ErrorLevel
	envDefault := GetEnvDefault("DEBUG", "false")
	if envDefault == "true" {
		loggingLevel = zerolog.DebugLevel
	}
	zerolog.SetGlobalLevel(loggingLevel)

	// Start new gateway, starting docker connection, retrieving all minio instances,
	// initializing minio connection and create bucket used by application
	gateway, err := newGateway()
	if err != nil {
		log.Fatal().Msgf("Failed to initialize gateway: %v", err)
	}

	// Define server routing and url handlers
	r := mux.NewRouter()
	r.HandleFunc("/object/{id:[a-zA-Z0-9-_]{1,32}}", gateway.putObjectHandler).Methods("PUT")
	r.HandleFunc("/object/{id:[a-zA-Z0-9-_]{1,32}}", gateway.getObjectHandler).Methods("GET")
	http.Handle("/", r)

	// Define middleware to add logging
	r.Use(loggingMiddleware)
	// Attach the recovery middleware
	r.Use(RecoveryMiddleware)
	// Start server on port 3000
	serverPort := GetEnvDefault("SERVER_PORT", "3000")
	log.Info().Msg("Starting server on :3000")
	err = http.ListenAndServe(fmt.Sprintf(":%s", serverPort), nil)
	if err != nil {
		// OPS something went wrong
		log.Fatal().Msgf("Starting server on :%s error: %s", serverPort, err.Error())
	}
}

// getClientForID get id hash int and calculate minioClients slice index
// it return minio client instance
func (g *MinioClients) getClientForID(id string) MinioClientInterface {
	// Simple hash to distribute requests
	index := hashStringToInt(id) % len(g.minioClients)
	log.Debug().Msgf("getClientForID: ID => %s, index => %d", id, index)
	return g.minioClients[index]
}

// hashStringToInt calculates the CRC32 hash and returns it as an int integer.
func hashStringToInt(s string) int {
	h := crc32.NewIEEE()
	_, err := h.Write([]byte(s))
	if err != nil {
		log.Error().Msgf("Hash function error: %s", err.Error())
	}
	sum32 := h.Sum32()
	log.Debug().Msgf("hashStringToInt: %s => %d", s, sum32)
	return int(sum32)
}

// putObjectHandler get object id and body from r (*http.Request) put object into Minio instance and write
// response to w (http.ResponseWriter)
func (g *MinioClients) putObjectHandler(w http.ResponseWriter, r *http.Request) {
	// Get url param id
	vars := mux.Vars(r)
	id := vars["id"]
	// Get client for given id
	clientMinio := g.getClientForID(id)
	// Put object into minio client
	info, err := clientMinio.PutObject(context.Background(), GetEnvDefault("MINIO_BUCKET_NAME", "bucket"), id, r.Body, -1, minio.PutObjectOptions{})
	if err != nil {
		// OPS something went wrong
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Create json response with some details
	response := responsePutObject{
		Bucket: info.Bucket,
		Size:   info.Size,
		Sha256: info.ChecksumSHA256,
	}
	payload, err := json.Marshal(response)
	if err != nil {
		// OPS something went wrong
		log.Error().Msgf("An error occured while creating response json: %s", err.Error())
	}

	// Return response as json
	w.Header().Set("Content-Type", "application/json")
	_, err = w.Write(payload)
	if err != nil {
		// OPS something went wrong
		log.Error().Msgf("An error occured while writing response: %s", err.Error())
	}

}

func (g *MinioClients) getObjectHandler(w http.ResponseWriter, r *http.Request) {
	// Get url param id
	vars := mux.Vars(r)
	id := vars["id"]

	// Get client for given id
	clientMinio := g.getClientForID(id)

	// GET object from minio client
	obj, err := clientMinio.GetObject(context.Background(), GetEnvDefault("MINIO_BUCKET_NAME", "bucket"), id, minio.GetObjectOptions{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer func(obj *minio.Object) {
		err := obj.Close()
		if err != nil {
			log.Error().Msgf("An error occured while closing object: %s", err.Error())
		}
	}(obj)
	_, err = obj.Stat()
	if err != nil {
		http.Error(w, "Object not found", http.StatusNotFound)
		return
	}
	_, err = io.Copy(w, obj)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func newGateway() (*MinioClients, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	bucketName := GetEnvDefault("MINIO_BUCKET_NAME", "bucket")
	minioPort := GetEnvDefault("MINIO_SERVICE_PORT", "9000")
	containers, err := cli.ContainerList(context.Background(), container.ListOptions{})
	if err != nil {
		return nil, err
	}

	var minioClients []MinioClientInterface

	for _, containerF := range containers {

		if strings.Contains(containerF.Names[0], "object-storage-amazing-object-storage-node") {
			log.Debug().Msgf("Container Minio -> %s", containerF.Names[0])
			containerJSON, err := cli.ContainerInspect(context.Background(), containerF.ID)
			if err != nil {
				log.Error().Msgf("Error inspecting container: %v", err)
			}

			// Extract environment variables
			envVars := containerJSON.Config.Env
			var accessKey, secretKey string

			// Desired environment variable keyAccessKey and keySecretKey
			keyAccessKey := "MINIO_ACCESS_KEY"
			keySecretKey := "MINIO_SECRET_KEY"

			// Find and print the value of the MINIO_ACCESS_KEY and MINIO_SECRET_KEY environment variable
			for _, env := range envVars {
				if strings.HasPrefix(env, keyAccessKey+"=") {
					accessKey = strings.TrimPrefix(env, keyAccessKey+"=")
				}
				if strings.HasPrefix(env, keySecretKey+"=") {
					secretKey = strings.TrimPrefix(env, keySecretKey+"=")
				}
			}

			// Initialize new minio client connection
			addr := fmt.Sprintf("%s:%s", containerF.NetworkSettings.Networks["object-storage_amazing-object-storage"].IPAddress, minioPort)
			minioClient, err := minio.New(addr, &minio.Options{
				Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
				Secure: false,
			})

			if err != nil {
				log.Error().Msgf("Error initializing Minio client: %s SKIP IT", err)
				continue
			}

			// Create bucket used by the app
			err = minioClient.MakeBucket(ctx, bucketName, minio.MakeBucketOptions{})
			if err != nil {
				// Check to see if we already own this bucket (which happens if you run this twice)
				exists, errBucketExists := minioClient.BucketExists(ctx, bucketName)
				if errBucketExists == nil && exists {
					log.Debug().Msgf("We already own %s", bucketName)
				} else {
					log.Fatal().Msg(err.Error())
				}
			} else {
				log.Printf("Successfully created bucket %s\n", bucketName)
			}
			minioClients = append(minioClients, minioClient)
		}
	}

	// If not minio client found return error
	if len(minioClients) == 0 {
		return nil, fmt.Errorf("no Minio clients found")
	}
	// Return struct with all minio clients found
	return &MinioClients{minioClients: minioClients}, nil
}

// RecoveryMiddleware is a middleware that recovers from panics and writes a 500 error response.
func RecoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				// Log the error and stack trace
				log.Printf("panic: %v\n%s", err, debug.Stack())
				// Return a 500 Internal Server Error response
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			}
		}()
		// Call the next handler
		next.ServeHTTP(w, r)
	})
}

// loggingMiddleware to create a log foreach request managed by server
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Create log with form "METHOD: %s, URL: %s"
		log.Debug().Msgf("METHOD: %s, URL: %s", r.Method, r.RequestURI)
		// Call the next handler, which can be another middleware in the chain, or the final handler.
		next.ServeHTTP(w, r)
	})
}

// GetEnvDefault get back environment variable by its key and if not it's set
// return default value provided
func GetEnvDefault(key, defVal string) string {
	// check if env var is set
	val, ex := os.LookupEnv(key)
	// otherwise return default value
	if !ex {
		return defVal
	}
	// return anv variable
	return val
}
