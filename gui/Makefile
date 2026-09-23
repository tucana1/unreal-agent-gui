BIN := bin/unreal-agent-gui
APP := bin/Unreal Agent.app
APPS ?= $(HOME)/Applications

.PHONY: build run test check app install-app clean

build:
	go build -trimpath -ldflags "-s -w" -o $(BIN) .

run: build
	./$(BIN)

test:
	go test ./...

check:
	go vet ./...
	@test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }

# macOS app bundle: a double-clickable Unreal Agent.app.
app: build
	rm -rf "$(APP)"
	mkdir -p "$(APP)/Contents/MacOS" "$(APP)/Contents/Resources"
	cp packaging/Info.plist "$(APP)/Contents/Info.plist"
	cp $(BIN) "$(APP)/Contents/MacOS/unreal-agent-gui"
	./packaging/icon.sh packaging/icon.png "$(APP)/Contents/Resources/icon.icns"
	codesign --force --sign - "$(APP)" 2>/dev/null || true

install-app: app
	mkdir -p "$(APPS)"
	rm -rf "$(APPS)/Unreal Agent.app"
	cp -R "$(APP)" "$(APPS)/"
	@echo "Installed $(APPS)/Unreal Agent.app"

clean:
	rm -rf bin
