package main

import (
    "context"
    "fmt"
    "os"
    "time"

    "field-service/internal/authn"
    "field-service/internal/jwks"
)

func main() {
    tok := os.Args[1]
    cache := jwks.New(os.Args[2], jwks.WithInterval(time.Second))
    v, err := authn.NewValidator(cache, "chisimba-api", "chisimba")
    if err != nil { panic(err) }
    p, err := v.Validate(tok)
    fmt.Println("principal", p.Subject, p.Type, p.Scope, p.Grants, p.Epoch, "err", err)
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    _ = ctx
}
