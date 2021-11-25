package main

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/gorilla/mux"
	"github.com/pkg/errors"
)

var (
	narXzPattern   = regexp.MustCompile(`\A[0-9a-df-np-sv-z]{52}\.nar\.xz\z`)
	narinfoPattern = regexp.MustCompile(`\A[0-9a-df-np-sv-z]{32}\.narinfo\z`)
)

func (proxy *Proxy) router() *mux.Router {
	r := mux.NewRouter()
	r.NotFoundHandler = notFound{}
	r.Use(loggingMiddleware)

	// public cache
	r.HandleFunc("/nix-cache-info", proxy.nixCacheInfo).Methods("GET")
	r.HandleFunc("/{key}", proxy.narinfoGet).Methods("GET")
	r.HandleFunc("/nar/{key}", proxy.narHead).Methods("HEAD")
	r.HandleFunc("/nar/{key}", proxy.narGet).Methods("GET")

	// S3 compat endpoints used by `nix copy`
	r.HandleFunc("/{bucket:[a-z-]+}/nix-cache-info", proxy.nixCacheInfo).Methods("GET")

	narinfo := "/{bucket:[a-z-]+}/{key}"
	r.HandleFunc(narinfo, proxy.narinfoPut).Methods("PUT")
	r.HandleFunc(narinfo, proxy.narinfoGet).Methods("GET")

	nar := `/{bucket:[a-z-]+}/nar/{key:[0-9a-df-np-sv-z]{52}\.nar(?:\.(?:xz|bz2|zst|lzip|lz4|br))?}`
	r.HandleFunc(nar, proxy.narHead).Methods("HEAD")
	r.HandleFunc(nar, proxy.narPut).Methods("PUT")
	r.HandleFunc(nar, proxy.narGet).Methods("GET")

	return r
}

func (proxy *Proxy) narinfoPath(r *http.Request) (string, string, error) {
	key, ok := mux.Vars(r)["key"]
	if ok && narinfoPattern.MatchString(key) {
		return filepath.Join(proxy.Dir, key), key, nil
	} else {
		return "", "", errors.New("Invalid narinfo name")
	}
}

func (proxy *Proxy) narPath(r *http.Request) (string, string, error) {
	key, ok := mux.Vars(r)["key"]
	if ok && narXzPattern.MatchString(key) {
		return filepath.Join(proxy.Dir, "nar", key), filepath.Join("nar", key), nil
	} else {
		return "", "", errors.New("Invalid nar name")
	}
}

func (proxy *Proxy) nixCacheInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Content-Type", "text/x-nix-cache-info")
	w.WriteHeader(200)
	_, _ = w.Write([]byte(`StoreDir: /nix/store
WantMassQuery: 1
Priority: 30`))
}

func (proxy *Proxy) narHead(w http.ResponseWriter, r *http.Request) {
	path, _, err := proxy.narPath(r)
	if badRequest(w, err) {
		return
	}

	if _, err := os.Stat(path); err != nil {
		w.Header().Add("Content-Type", "text/html")
		w.WriteHeader(404)
	} else {
		w.WriteHeader(200)
	}
}

func (proxy *Proxy) narPut(w http.ResponseWriter, r *http.Request) {
	path, s3Path, err := proxy.narPath(r)
	if badRequest(w, errors.WithMessage(err, "Calculating nar path")) {
		return
	}

	fdw, err := os.Create(path)
	if internalServerError(w, errors.WithMessagef(err, "Creating path %q", path)) {
		return
	}
	defer fdw.Close()

	_, err = io.Copy(fdw, r.Body)
	if internalServerError(w, errors.WithMessage(err, "Copying body")) {
		return
	}

	if proxy.uploader != nil {
		f, err := os.Open(path)
		if err != nil {
			log.Panicf("failed to open file %q, %v", path, err)
		}
		defer f.Close()

		input := &s3manager.UploadInput{Bucket: aws.String(proxy.BucketName), Key: aws.String(s3Path), Body: f}
		result, err := proxy.uploader.Upload(input)
		if err != nil {
			log.Panicf("failed to upload file, %v", err)
		}
		log.Printf("file uploaded to %q\n", aws.StringValue(&result.Location))
	}

	w.WriteHeader(200)
}

func (proxy *Proxy) narGet(w http.ResponseWriter, r *http.Request) {
	path, _, err := proxy.narPath(r)
	if badRequest(w, err) {
		return
	}

	if _, err := os.Stat(path); err != nil {
		w.Header().Add("Content-Type", "text/html")
		w.WriteHeader(404)
		_, _ = w.Write([]byte("404"))
		return
	}

	w.Header().Add("Content-Type", "application/x-nix-nar")
	http.ServeFile(w, r, path)
}

func (proxy *Proxy) narinfoPut(w http.ResponseWriter, r *http.Request) {
	path, s3Path, err := proxy.narinfoPath(r)
	if badRequest(w, err) {
		return
	}

	body, err := io.ReadAll(r.Body)
	if badRequest(w, err) {
		return
	}

	info := &NarInfo{}
	if badRequest(w, info.Unmarshal(bytes.NewBuffer(body))) {
		return
	}

	if internalServerError(w, info.Sign(proxy.privateKey)) {
		return
	}

	signed := &bytes.Buffer{}
	if internalServerError(w, info.Marshal(signed)) {
		return
	}

	if internalServerError(w, os.WriteFile(path, signed.Bytes(), 0644)) {
		return
	}

	if proxy.uploader != nil {
		_, err = proxy.uploader.Upload(&s3manager.UploadInput{
			Bucket: aws.String(proxy.BucketName),
			Key:    aws.String(s3Path),
			Body:   signed,
		})

		if internalServerError(w, err) {
			return
		}
	}

	w.WriteHeader(200)
}

func (proxy *Proxy) narinfoGet(w http.ResponseWriter, r *http.Request) {
	path, _, err := proxy.narinfoPath(r)
	if badRequest(w, err) {
		return
	}

	if _, err := os.Stat(path); err != nil {
		vars := mux.Vars(r)
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<Error>
  <Code>NoSuchKey</Code>
  <Message>The specified key does not exist.</Message>
  <Resource>` + r.URL.Path + `</Resource>
  <BucketName>` + vars["bucket"] + `</BucketName>
  <Key>` + vars["key"] + `</Key>
  <RequestId>16B81914FBB8345F</RequestId>
  <HostId>672a09d6-39bb-41a6-bcf3-b0375d351cfe</HostId>
</Error>`))
		return
	}

	w.Header().Add("Content-Type", "text/x-nix-narinfo")
	http.ServeFile(w, r, path)
}
