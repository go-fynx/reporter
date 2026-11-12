module github.com/go-fynx/reporter

go 1.27

require (
	connectrpc.com/connect/v2 v2.0.2
	github.com/go-fynx/reporter/reporter-go v0.0.0
	go.mongodb.org/mongo-driver/v2 v2.10.0
	golang.org/x/time v0.16.0
	google.golang.org/protobuf v1.36.12
)

require (
	github.com/klauspost/compress v1.20.3 // indirect
	github.com/xdg-go/scram v1.4.0 // indirect
	github.com/xdg-go/stringprep v1.0.4 // indirect
	github.com/youmark/pkcs8 v0.0.0-20240726163527-a2c0da244d78 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/text v0.41.0 // indirect
)

replace github.com/go-fynx/reporter/reporter-go => ./reporter-go
