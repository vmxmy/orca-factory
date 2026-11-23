module github.com/vmxmy/orca-factory/driver

go 1.25

toolchain go1.27.4

require github.com/BurntSushi/toml v1.6.0

require go.uber.org/goleak v1.3.0

require golang.org/x/net v0.26.0 // indirect

require (
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/gorilla/websocket v1.5.4-0.20240701034025-d67f41855da4
	github.com/larksuite/oapi-sdk-go/v3 v3.13.0
)
