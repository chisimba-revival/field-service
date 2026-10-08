package main

import (
    "context"
    "fmt"
    "io"
    "net/http"
    "os"
    "time"
)

func main() {
    req, _ := http.NewRequestWithContext(context.Background(), "GET", os.Args[1], nil)
    req.Header.Set("Accept", "application/json")
    req.Header.Set("Cache-Control", "no-cache")
    client := &http.Client{Timeout: 5*time.Second}
    resp, err := client.Do(req)
    if err != nil { fmt.Println("ERR", err); return }
    defer resp.Body.Close()
    b, _ := io.ReadAll(resp.Body)
    fmt.Println(resp.StatusCode, len(b), string(b[:min(200, len(b))]))
}
func min(a,b int) int { if a<b {return a}; return b }
