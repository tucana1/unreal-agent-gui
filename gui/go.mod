module github.com/tucana1/unreal-agent-gui/gui

go 1.27.0

require (
	github.com/ledongthuc/pdf v0.0.0-20260907135840-6c8c28e0e8a0
	github.com/unreallabsai/unreal-agent v0.0.0
	github.com/webview/webview_go v0.0.0-20240831120633-6173450d4dd6
	golang.org/x/net v0.59.0
)

require (
	github.com/apapsch/go-jsonmerge/v2 v2.0.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/oapi-codegen/runtime v1.6.0 // indirect
	golang.org/x/image v0.46.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
)

// Always build against the upstream harness checked out in this fork.
replace github.com/unreallabsai/unreal-agent => ../
