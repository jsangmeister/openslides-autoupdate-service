package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"syscall"

	"github.com/openslides/openslides-autoupdate-service/internal/autoupdate"
	"github.com/openslides/openslides-autoupdate-service/internal/datastore"
	autoupdateHttp "github.com/openslides/openslides-autoupdate-service/internal/http"
	"github.com/openslides/openslides-autoupdate-service/internal/redis"
	"github.com/openslides/openslides-autoupdate-service/internal/restrict"
	"github.com/openslides/openslides-autoupdate-service/internal/test"
)

const (
	generalCertName = "cert.pem"
	generalKeyName  = "key.pem"
	specialCertName = "autoupdate.pem"
	specialKeyName  = "autoupdate-key.pem"
)

func main() {
	closed := make(chan struct{})

	errHandler := func(err error) {
		log.Printf("Error: %v", err)
	}

	// Datastore Service.
	datastoreService, err := buildDatastore(closed, errHandler)
	if err != nil {
		log.Fatalf("Can not create datastore service: %v", err)
	}

	// Perm Service.
	perms := &test.MockPermission{}
	perms.Default = true

	// Restricter Service.
	restricter := restrict.New(perms, restrict.OpenSlidesChecker(perms))

	// Autoupdate Service.
	service := autoupdate.New(datastoreService, restricter, closed)

	// Auth Service.
	authService := buildAuth()

	// HTTP Hanlder.
	handler := autoupdateHttp.New(service, authService)

	// Create tls http2 server.
	cert, err := getCert()
	if err != nil {
		log.Fatalf("Can not get certificate: %v", err)
	}

	listenAddr := getEnv("AUTOUPDATE_HOST", "") + ":" + getEnv("AUTOUPDATE_PORT", "9012")
	srv := &http.Server{Addr: listenAddr, Handler: handler}
	tlsConf := new(tls.Config)
	tlsConf.NextProtos = []string{"h2"}
	tlsConf.Certificates = []tls.Certificate{cert}

	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatalf("Can not listen on %s: %v", listenAddr, err)
	}
	defer ln.Close()

	tlsListener := tls.NewListener(ln, tlsConf)

	// Shutdown logig in separate goroutine.
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		waitForShutdown()

		close(closed)
		if err := srv.Shutdown(context.Background()); err != nil {
			log.Printf("Error on HTTP server shutdown: %v", err)
		}
	}()

	fmt.Printf("Listen on %s\n", listenAddr)
	if err := srv.Serve(tlsListener); err != http.ErrServerClosed {
		log.Fatalf("HTTP Server Error: %v", err)
	}
	<-shutdownDone
}

func getCert() (tls.Certificate, error) {
	certDir := getEnv("CERT_DIR", "")
	if certDir == "" {
		cert, err := autoupdateHttp.GenerateCert()
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("creating new certificate: %w", err)
		}
		fmt.Println("Use inmemory self signed certificate")
		return cert, nil
	}
	certFile := path.Join(certDir, specialCertName)
	if _, err := os.Stat(certFile); os.IsNotExist(err) {
		certFile2 := path.Join(certDir, generalCertName)
		if _, err := os.Stat(certFile); os.IsNotExist(err) {
			return tls.Certificate{}, fmt.Errorf("%s or %s has to exist", certFile, certFile2)
		}
		certFile = certFile2
	}

	keyFile := path.Join(certDir, specialKeyName)
	if _, err := os.Stat(keyFile); os.IsNotExist(err) {
		keyFile2 := path.Join(certDir, generalKeyName)
		if _, err := os.Stat(keyFile); os.IsNotExist(err) {
			return tls.Certificate{}, fmt.Errorf("%s or %s has to exist", keyFile, keyFile2)
		}
		keyFile = keyFile2
	}

	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("loading certificates from %s and %s: %w", certFile, keyFile, err)
	}
	fmt.Printf("Use certificate %s with key %s\n", certFile, keyFile)

	return cert, nil
}

// waitForShutdown blocks until the service exists.
//
// It listens on SIGINT and SIGTERM. If the signal is received for a second
// time, the process is killed with statuscode 1.
func waitForShutdown() {
	sigint := make(chan os.Signal, 1)
	// syscall.SIGTERM is not pressent on all plattforms. Since the autoupdate
	// service is only run on linux, this is ok. If other plattforms should be
	// supported, os.Interrupt should be used instead.
	signal.Notify(sigint, syscall.SIGINT, syscall.SIGTERM)
	<-sigint
	go func() {
		<-sigint
		os.Exit(1)
	}()
}

// buildDatastore builds the datastore implementation needed by the autoupdate
// service. It uses environment variables to make the decission. Per default, a
// fake server is started and its url is used.
func buildDatastore(closed <-chan struct{}, errHandler func(error)) (autoupdate.Datastore, error) {
	var f *faker
	var url string
	dsService := getEnv("DATASTORE", "fake")
	switch dsService {
	case "fake":
		fmt.Println("Fake Datastore")
		f = newFaker(os.Stdin)
		url = f.ts.TS.URL

	case "service":
		host := getEnv("DATASTORE_READER_HOST", "localhost")
		port := getEnv("DATASTORE_READER_PORT", "9010")
		protocol := getEnv("DATASTORE_READER_PROTOCOL", "http")
		url = protocol + "://" + host + ":" + port

	default:
		return nil, fmt.Errorf("unknown datastore %s", dsService)
	}

	fmt.Println("Datastore URL:", url)
	receiver, err := buildReceiver(f)
	if err != nil {
		return nil, fmt.Errorf("build receiver: %w", err)
	}
	return datastore.New(url, closed, errHandler, receiver), nil
}

// buildReceiver builds the receiver needed by the datastore service. It uses
// environment variables to make the decission. Per default, the given faker is
// used.
func buildReceiver(f *faker) (datastore.Updater, error) {
	var receiver datastore.Updater
	serviceName := getEnv("MESSAGING", "fake")
	switch serviceName {
	case "redis":
		redisAddress := getEnv("MESSAGE_BUS_HOST", "localhost") + ":" + getEnv("MESSAGE_BUS_PORT", "6379")
		conn := redis.NewConnection(redisAddress)
		if getEnv("REDIS_TEST_CONN", "true") == "true" {
			if err := conn.TestConn(); err != nil {
				return nil, fmt.Errorf("connect to redis: %w", err)
			}
		}
		receiver = &redis.Service{Conn: conn}

	case "fake":
		receiver = f
		if f == nil {
			serviceName = "none"
		}
	default:
		return nil, fmt.Errorf("unknown messagin service %s", serviceName)
	}

	fmt.Printf("Messaging Service: %s\n", serviceName)
	return receiver, nil
}

// buildAuth returns the auth service needed by the http server.
//
// Currently, there is only the fakeAuth service.
func buildAuth() autoupdateHttp.Authenticator {
	return fakeAuth(1)
}

// getEnv returns the value of the environment variable env. If it is empty, the
// defaultValue is used.
func getEnv(env, devaultValue string) string {
	value := os.Getenv(env)
	if value == "" {
		return devaultValue
	}
	return value
}
