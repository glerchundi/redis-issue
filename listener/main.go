package main

import (
	"log"
	"net/http"
	"strings"
	"time"
	"net"
	"os"
	//"strconv"

	"gopkg.in/redis.v5"
	"gopkg.in/tylerb/graceful.v1"
	"github.com/glerchundi/redis-issue/listener/util"
	common "github.com/glerchundi/redis-issue/util"
	"github.com/gorilla/websocket"
	"github.com/spf13/pflag"
)

var (
	redisClient *redis.Client

	upgrader websocket.Upgrader
	waitGroup *common.WaitGroup
	quitting chan struct{}
)

type httpError struct {
	statusCode int
}

func (*httpError) Error() string {
	return ""
}

func websocketHandler(w http.ResponseWriter, r *http.Request) error {
	id := strings.TrimLeft(r.URL.Path, "/")

	/*
	var sleepMs time.Duration = -1
	if n, err := strconv.Atoi(strings.Split(id, "-")[0]); err == nil && n > 0 {
		sleepMs = time.Duration(n)
	}
	*/

	waitGroup.Add(1)
	defer waitGroup.Done()

	var dlock util.Lock
	var ttl time.Duration = 10 * time.Second
	var psclient util.PubSubClient
	if redisClient != nil {
		dlock := util.NewRedLock(redisClient, id, ttl)

		ok, err := dlock.Lock()
		if err != nil {
			return &httpError{http.StatusInternalServerError}
		}
		defer redisClient.Close()
		defer func() {
			/*
			if sleepMs  0  {
				time.Sleep(sleepMs * 10 * time.Millisecond)
			}
			*/
			if err := dlock.Unlock(); err != nil {
				log.Printf("failed to unlock dlock for '%s': %v\n", id, err)
			}
		}()

		if !ok {
			return &httpError{http.StatusForbidden}
		}

		psclient = util.NewRedPubSubClient(redisClient)
	} else {
		dlock = util.NewMockLock()
		psclient = util.NewMockPubSubClient()
	}

	err := psclient.Run(id)
	if err != nil {
		return err
	}
	defer psclient.Close()

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return err
	}

	wsclient := common.NewWebSocketClient(conn)
	wsclient.Run()
	defer wsclient.Close()

	log.Printf("Client connected to: %s\n", r.URL)
	defer log.Printf("Client disconnected from: %s\n", r.URL)

	ticker := time.NewTicker((ttl * 9) / 10)
	defer ticker.Stop()

	for {
		select {
		case <-quitting:
			return nil
		case <-ticker.C:
			if ok, err := dlock.Renew(); err != nil {
				return err
			} else if !ok {
				log.Println("unable to renew lock")
				return nil
			}
		case _, ok := <-psclient.ReadMessage():
			if !ok {
				log.Println("redis pubsub channel closed")
				return nil
			}
		case _, ok := <-wsclient.ReadMessage():
			if !ok {
				log.Println("websocket channel closed")
				return nil
			}
		}
	}

	return nil
}

func printInterfaces() {
	ifaces, err := net.Interfaces()
	if err != nil {
		return
	}

	for _, i := range ifaces {
		addrs, _ := i.Addrs()
		if err == nil {
			for _, addr := range addrs {
				var ip net.IP
				switch v := addr.(type) {
				case *net.IPNet:
					ip = v.IP
				case *net.IPAddr:
					ip = v.IP
				}
				log.Println(ip)
			}
		}
	}
}

func main() {
	var addr string = "127.0.0.1:6379"
	var poolSize int = 5000

	fs := pflag.NewFlagSet(os.Args[0], pflag.ExitOnError)
	fs.StringVar(&addr, "addr", addr, "")
	fs.IntVar(&poolSize, "pool-size", poolSize, "")

	fs.Parse(os.Args[1:])

	printInterfaces()

	if addr != "" {
		redisClient = redis.NewClient(&redis.Options{
			Addr:     addr,
			Password: "",
			DB:       0,
			PoolSize: poolSize,
		})

		_, err := redisClient.Ping().Result()
		if err != nil {
			log.Fatalf("%v", err)
		}
	}

	upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	waitGroup = common.NewWaitGroup()
	quitting = make(chan struct{})

	log.Println("Listener started")
	defer log.Println("Listener stopped")

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if err := websocketHandler(w, r); err != nil {
			var statusCode int = http.StatusInternalServerError
			if he, ok := err.(*httpError); ok {
				statusCode = he.statusCode
			}
			w.WriteHeader(statusCode)
		}
	})

	server := &graceful.Server{
		Timeout: 10 * time.Second,
		Server: &http.Server{
			Addr:    ":8080",
			Handler: mux,
		},
	}

	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("%v\n", err)
	}
	close(quitting)

	err := waitGroup.WaitTimeout(10 * time.Second)
	if err != nil {
		log.Fatalf("%v", err)
	}
}
