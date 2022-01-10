package main

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8090"
	}

	fmt.Printf("Echo server listening on port %s.\n", port)

	err := http.ListenAndServe(
		":"+port,
		h2c.NewHandler(
			http.HandlerFunc(handler),
			&http2.Server{},
		),
	)
	if err != nil {
		panic(err)
	}
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool {
		return true
	},
}

func handler(wr http.ResponseWriter, req *http.Request) {
	defer req.Body.Close()

	if os.Getenv("LOG_HTTP_BODY") != "" || os.Getenv("LOG_HTTP_HEADERS") != "" {
		fmt.Printf("--------  %s | %s %s\n", req.RemoteAddr, req.Method, req.URL)
	} else {
		fmt.Printf("%s | %s %s\n", req.RemoteAddr, req.Method, req.URL)
	}

	if os.Getenv("LOG_HTTP_HEADERS") != "" {
		fmt.Printf("Headers\n")
		//Iterate over all header fields
		for k, v := range req.Header {
			fmt.Printf("%q : %q\n", k, v)
		}
	}

	if os.Getenv("LOG_HTTP_BODY") != "" {
		buf := &bytes.Buffer{}
		buf.ReadFrom(req.Body) // nolint:errcheck

		if buf.Len() != 0 {
			w := hex.Dumper(os.Stdout)
			w.Write(buf.Bytes()) // nolint:errcheck
			w.Close()
		}

		// Replace original body with buffered version so it's still sent to the
		// browser.
		req.Body.Close()
		req.Body = ioutil.NopCloser(
			bytes.NewReader(buf.Bytes()),
		)
	}

	sendServerHostnameString := os.Getenv("SEND_SERVER_HOSTNAME")
	if v := req.Header.Get("X-Send-Server-Hostname"); v != "" {
		sendServerHostnameString = v
	}

	sendServerHostname := !strings.EqualFold(
		sendServerHostnameString,
		"false",
	)

	if websocket.IsWebSocketUpgrade(req) {
		serveWebSocket(wr, req, sendServerHostname)
	} else if req.URL.Path == "/.ws" {
		wr.Header().Add("Content-Type", "text/html")
		wr.WriteHeader(200)
		io.WriteString(wr, websocketHTML) // nolint:errcheck
	} else if req.URL.Path == "/.sse" {
		serveSSE(wr, req, sendServerHostname)
	} else {
		serveHTTP(wr, req, sendServerHostname)
	}
}

func serveWebSocket(wr http.ResponseWriter, req *http.Request, sendServerHostname bool) {
	connection, err := upgrader.Upgrade(wr, req, nil)
	if err != nil {
		fmt.Printf("%s | %s\n", req.RemoteAddr, err)
		return
	}

	defer connection.Close()
	fmt.Printf("%s | upgraded to websocket\n", req.RemoteAddr)

	var message []byte

	if sendServerHostname {
		host, err := os.Hostname()
		if err == nil {
			message = []byte(fmt.Sprintf("Request served by %s", host))
		} else {
			message = []byte(fmt.Sprintf("Server hostname unknown: %s", err.Error()))
		}
	}

	err = connection.WriteMessage(websocket.TextMessage, message)
	if err == nil {
		var messageType int

		for {
			messageType, message, err = connection.ReadMessage()
			if err != nil {
				break
			}

			if messageType == websocket.TextMessage {
				fmt.Printf("%s | txt | %s\n", req.RemoteAddr, message)
			} else {
				fmt.Printf("%s | bin | %d byte(s)\n", req.RemoteAddr, len(message))
			}

			err = connection.WriteMessage(messageType, message)
			if err != nil {
				break
			}
		}
	}

	if err != nil {
		fmt.Printf("%s | %s\n", req.RemoteAddr, err)
	}
}

func serveHTTP(wr http.ResponseWriter, req *http.Request, sendServerHostname bool) {
	//wr.Header().Add("Content-Type", "text/plain")
	wr.Header().Add("Content-Type", "application/json")
	//wr.WriteHeader(200)
	randomFail := rand.Int()%2 != 0

	if randomFail {
		wr.WriteHeader(403)
		fmt.Println("return 403")
	} else {
		fmt.Println("return 200")
		wr.WriteHeader(200)
	}

	// if sendServerHostname {
	// 	host, err := os.Hostname()
	// 	if err == nil {
	// 		fmt.Fprintf(wr, "Request served by %s\n\n", host)
	// 	} else {
	// 		fmt.Fprintf(wr, "Server hostname unknown: %s\n\n", err.Error())
	// 	}
	// }

	writeRequest(wr, req)
}

func serveSSE(wr http.ResponseWriter, req *http.Request, sendServerHostname bool) {
	if _, ok := wr.(http.Flusher); !ok {
		http.Error(wr, "Streaming unsupported!", http.StatusInternalServerError)
		return
	}

	var echo strings.Builder
	writeRequest(&echo, req)

	wr.Header().Set("Content-Type", "text/event-stream")
	wr.Header().Set("Cache-Control", "no-cache")
	wr.Header().Set("Connection", "keep-alive")
	wr.Header().Set("Access-Control-Allow-Origin", "*")

	var id int

	// Write an event about the server that is serving this request.
	if sendServerHostname {
		if host, err := os.Hostname(); err == nil {
			writeSSE(
				wr,
				req,
				&id,
				"server",
				host,
			)
		}
	}

	// Write an event that echoes back the request.
	writeSSE(
		wr,
		req,
		&id,
		"request",
		echo.String(),
	)

	// Then send a counter event every second.
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-req.Context().Done():
			return
		case t := <-ticker.C:
			writeSSE(
				wr,
				req,
				&id,
				"time",
				t.Format(time.RFC3339),
			)
		}
	}
}

// writeSSE sends a server-sent event and logs it to the console.
func writeSSE(
	wr http.ResponseWriter,
	req *http.Request,
	id *int,
	event, data string,
) {
	*id++
	writeSSEField(wr, req, "event", event)
	writeSSEField(wr, req, "data", data)
	writeSSEField(wr, req, "id", strconv.Itoa(*id))
	fmt.Fprintf(wr, "\n")
	wr.(http.Flusher).Flush()
}

// writeSSEField sends a single field within an event.
func writeSSEField(
	wr http.ResponseWriter,
	req *http.Request,
	k, v string,
) {
	for _, line := range strings.Split(v, "\n") {
		fmt.Fprintf(wr, "%s: %s\n", k, line)
		fmt.Printf("%s | sse | %s: %s\n", req.RemoteAddr, k, line)
	}
}

// writeRequest writes request headers to w.
func writeRequest(w io.Writer, req *http.Request) {
	// fmt.Fprintf(w, "%s %s %s\n", req.Proto, req.Method, req.URL)
	// fmt.Fprintln(w, "")

	// fmt.Fprintf(w, "Host: %s\n", req.Host)
	// for key, values := range req.Header {
	// 	for _, value := range values {
	// 		fmt.Fprintf(w, "%s: %s\n", key, value)
	// 	}
	// }
	// println("about to sleep")
	// time.Sleep(5 * time.Second)
	// println("slept 5 seconds")

	var body bytes.Buffer
	//io.Copy(&body, bytes.NewReader([]byte("{\"access_token\": \"eyJhbGciOiJSUzI1NiIsInR5cCIgOiAiSldUIiwia2lkIiA6ICJGTzFNRUhJWGpWSklMMkptSm5xWkRYQllWdzJTOHk3MElqZ0pPSFpMakZFIn0.eyJleHAiOjE2MjY3ODEzMzYsImlhdCI6MTYyNjc4MTAzNiwiYXV0aF90aW1lIjoxNjI2NzgwOTYyLCJqdGkiOiI2NTQ2MTNhNi05ZjFjLTQ3MzUtODEwMy1jMzZhNzE4NmEzNjgiLCJpc3MiOiJodHRwczovL2F1dGgtc2VydmljZS5zZXJ2aWNlLmRldi5mZnAtZGV2LmNvbS9hdXRoL3JlYWxtcy9yZXdlLWZ1bGZpbGxtZW50IiwiYXVkIjoibG9jYXRpb24tY29ubmVjdG9yLWJhY2tvZmZpY2UiLCJzdWIiOiI0OWIyY2Q5Ny02YjNkLTRjODAtYTY0Ny0zYTYxOWU1OWUxZGIiLCJ0eXAiOiJCZWFyZXIiLCJhenAiOiJsb2NhdGlvbi1iYWNrb2ZmaWNlIiwibm9uY2UiOiJOMC4wODc1MzM5ODI1OTAyOTEyMTE2MjY3ODEwMzYyNTQiLCJzZXNzaW9uX3N0YXRlIjoiMzBkYThhZTgtMjBjYy00MmM1LWIxYzItNTdhOWI5MTBjMmY4IiwiYWNyIjoiMCIsImFsbG93ZWQtb3JpZ2lucyI6WyJodHRwOi8vbG9jYWxob3N0OjQyMDAiLCJodHRwczovL3Jld2Uuc2VydmljZS5kZXYuZmZwLWRldi5jb20iXSwicmVhbG1fYWNjZXNzIjp7InJvbGVzIjpbIlNSLWxvY2F0aW9uLWNvbm5lY3Rvci1iYWNrb2ZmaWNlLXNlcnZpY2Vsb2NhdGlvbnMtYWRtaW4iLCJTUi1sb2NhdGlvbi1jb25uZWN0b3ItYmFja29mZmljZS1hZG1pbiJdfSwicmVzb3VyY2VfYWNjZXNzIjp7ImxvY2F0aW9uLWNvbm5lY3Rvci1iYWNrb2ZmaWNlIjp7InJvbGVzIjpbImRvd25sb2FkX2xvY2F0aW9ucyIsImluaXRpYWxpemUtd2FyZWhvdXNlcyIsInVwbG9hZF9sb2NhdGlvbnMiLCJkZWxldGVfbG9jYXRpb25zIiwiZWJvb3N0ZXItaW1wb3J0Iiwidmlld19sb2NhdGlvbnMiLCJ1cGxvYWRfbG9jYXRpb25zX2RpbWVuc2lvbnMiLCJjcmVhdGVfbG9jYXRpb25zIiwiYWRtaW5fc2VydmljZV9sb2NhdGlvbnMiXX0sImxvY2F0aW9uLWJhY2tvZmZpY2UiOnsicm9sZXMiOlsiYmFja29mZmljZS1wb3J0YWwtdmlldy1hcHAiXX19LCJzY29wZSI6Im9wZW5pZCBwcm9maWxlIGVtYWlsIHN0YW5kYXJkIiwiZW1haWxfdmVyaWZpZWQiOmZhbHNlLCJsb2NhdGlvbnMiOlsiMjMxMDA2fEJlcmxpbjEiLCIyMzEwMTF8TcO8bmNoZW4iLCIyNDA1NTd8QmVybGluIDMiLCIyNDA1ODF8U3R1dHRnYXJ0IiwiMzIwNTA5fEZGQyAyLjAiLCIzMjA1MTZ8TWFubmhlaW0iXSwicHJlZmVycmVkX3VzZXJuYW1lIjoidGVhbS1hbHBoYSIsImxvY2FsZSI6ImRlIiwiZW1haWwiOiJkZXYtdGVhbS1hbHBoYUByZXdlLWRpZ2l0YWwuY29tIn0.CIdYP8l3kTfrkq0bv5daFjRKHxenuG60YBpbEkvpLiAmLqqZ8ZIohuO2SGA0UEHH7TBHS0gzae6Za0bapYq8dlOXDTEoTIn0439WkM6r2CZsrNBPN8T4aV7OeGxmDvzuRgltG2tTRHNmrFWYt8KrHu7SgNrX0lg990wHQgLitr1t_QihdZCSidMiUez8uUbVBBhB3qFyrshHF9OLzrfWIkkbI8TwS_HE-QAIxotnB5y3eV0rDRZGNHQ6SUt9UWoZ8G2fm_rNDMNEOsNmsRYIdQMF8wv4BcIZoftpaC3TlKB4UoZbg1Zk5QYYPl6neUandpIxIa1TGDcGc_5EOD42zQ\"}"))) // nolint:errcheck
	io.Copy(&body, bytes.NewReader([]byte("{\"access_token\": \"fake-token\", \"gtins\": [\"999\"], \"changesUntil\": \"2020-12-12T00:00:11.111Z\"}"))) // nolint:errcheck

	if body.Len() > 0 {
		fmt.Fprintln(w, "")
		body.WriteTo(w) // nolint:errcheck
	}
}
