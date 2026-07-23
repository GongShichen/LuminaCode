APP_NAME ?= lumina
BACKEND_NAME ?= lumina-backend
GO ?= go
CGO_ENABLED ?= 1
NPM ?= npm
BUILD_DIR ?= tmp
INSTALL_DIR ?= $(shell os=$$(uname -s); if [ "$$os" = "Darwin" ] && [ -d /opt/homebrew/bin ] && [ -w /opt/homebrew/bin ]; then printf /opt/homebrew/bin; elif [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then printf /usr/local/bin; else printf '%s/.local/bin' "$$HOME"; fi)
APP_ROOT ?= $(or $(LUMINA_APP_ROOT),$(HOME)/.lumina)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || printf dev)
PURGE ?= 0
SKIP_MANAGED_COMPONENTS ?= 0
SKIP_MEMORY_MODELS ?= 0
NO_PATH_UPDATE ?= 0
BUILD_PATH := $(BUILD_DIR)/$(APP_NAME)
BACKEND_BUILD_PATH := $(BUILD_DIR)/$(BACKEND_NAME)
INSTALL_PATH := $(INSTALL_DIR)/$(APP_NAME)
BACKEND_INSTALL_PATH := $(INSTALL_DIR)/$(BACKEND_NAME)

.PHONY: help build install _install-preflight _install-build _install-deploy uninstall purge doctor clean

help:
	@printf '%s\n' \
		'LuminaCode Makefile' \
		'' \
		'Targets:' \
		'  make build      Build the frontend launcher and Go backend' \
		'  make install    Install or atomically upgrade AppRoot' \
		'  make doctor     Inspect the resolved AppRoot and managed components' \
		'  make uninstall  Remove app/cache/state; preserve config/data/layout.json' \
		'  make purge      Remove the complete AppRoot, including user data' \
		'  make clean      Remove local build output' \
		'' \
		'Overrides:' \
		'  make install INSTALL_DIR=/usr/local/bin' \
		'  make install APP_ROOT=/opt/lumina' \
		'  make install NO_PATH_UPDATE=1' \
		'  make uninstall PURGE=1'

build:
	@mkdir -p "$(BUILD_DIR)"
	CGO_ENABLED=$(CGO_ENABLED) $(GO) build -o "$(BACKEND_BUILD_PATH)" .
	$(NPM) --prefix frontend install
	$(NPM) --prefix frontend run build
	@set -eu; \
	repo_frontend="$(CURDIR)/frontend"; \
	build_dir_abs="$(CURDIR)/$(BUILD_DIR)"; \
	configured_app_root="$(APP_ROOT)"; \
	{ \
		printf '%s\n' '#!/usr/bin/env sh'; \
		printf '%s\n' 'set -eu'; \
		printf '%s\n' 'script_dir="$$(CDPATH= cd -- "$$(dirname -- "$$0")" && pwd)"'; \
		printf '%s\n' 'if [ -x "$$script_dir/$(BACKEND_NAME)" ]; then export LUMINA_BACKEND_BIN="$$script_dir/$(BACKEND_NAME)"; fi'; \
		printf '%s\n' "if [ \"\$$script_dir\" = \"$$build_dir_abs\" ]; then"; \
		printf '%s\n' '  frontend_root="$${LUMINA_FRONTEND_ROOT:-'"$$repo_frontend"'}"'; \
		printf '%s\n' '  export LUMINA_RESOURCE_ROOT="$${LUMINA_RESOURCE_ROOT:-'"$(CURDIR)"'}"'; \
		printf '%s\n' 'else'; \
		printf '%s\n' '  export LUMINA_APP_ROOT="$${LUMINA_APP_ROOT:-'"$$configured_app_root"'}"'; \
		printf '%s\n' '  frontend_root="$${LUMINA_FRONTEND_ROOT:-$$LUMINA_APP_ROOT/app/frontend}"'; \
		printf '%s\n' '  export LUMINA_RESOURCE_ROOT="$${LUMINA_RESOURCE_ROOT:-$$LUMINA_APP_ROOT/app/resources}"'; \
		printf '%s\n' 'fi'; \
		printf '%s\n' 'if [ ! -f "$$frontend_root/dist/index.js" ]; then echo "LuminaCode frontend is missing: $$frontend_root" >&2; exit 1; fi'; \
		printf '%s\n' 'export NODE_PATH="$$frontend_root/node_modules$${NODE_PATH:+:$$NODE_PATH}"'; \
		printf '%s\n' 'exec node "$$frontend_root/dist/index.js" "$$@"'; \
	} > "$(BUILD_PATH)"; \
	chmod 0755 "$(BUILD_PATH)"

install:
	@MAKE="$(MAKE)" sh scripts/install.sh

_install-preflight:
	@chmod 0755 scripts/install-preflight.sh scripts/setup-memory-models.sh
	@LUMINA_APP_ROOT="$(APP_ROOT)" \
		SKIP_MANAGED_COMPONENTS="$(SKIP_MANAGED_COMPONENTS)" \
		SKIP_MEMORY_MODELS="$(SKIP_MEMORY_MODELS)" \
		scripts/install-preflight.sh

_install-build:
	@$(MAKE) build
	@sh scripts/build-bge-metal.sh

_install-deploy:
	@set -eu; \
	os="$$(uname -s)"; \
	case "$$os" in Darwin) os_name="macOS" ;; Linux) os_name="Linux" ;; *) echo "Unsupported OS: $$os"; exit 1 ;; esac; \
	case "$(APP_ROOT)" in /*) ;; *) echo "APP_ROOT must be absolute: $(APP_ROOT)"; exit 1 ;; esac; \
	if [ "$(APP_ROOT)" = "/" ]; then echo "Refusing unsafe APP_ROOT: $(APP_ROOT)"; exit 1; fi; \
	rc_file=""; \
	if [ "$(NO_PATH_UPDATE)" != "1" ]; then \
		shell_path="$${SHELL:-/bin/sh}"; \
		if [ "$$os" = "Darwin" ]; then detected="$$(dscl . -read "/Users/$$(id -un)" UserShell 2>/dev/null | awk '{print $$2}' || true)"; [ -z "$$detected" ] || shell_path="$$detected"; \
		else detected="$$(getent passwd "$$(id -un)" 2>/dev/null | cut -d: -f7 || true)"; [ -z "$$detected" ] || shell_path="$$detected"; fi; \
		shell_name="$$(basename "$$shell_path")"; \
		case "$$shell_name" in zsh) rc_file="$${ZDOTDIR:-$$HOME}/.zshrc" ;; bash) if [ "$$os" = "Darwin" ]; then rc_file="$$HOME/.bash_profile"; else rc_file="$$HOME/.bashrc"; fi ;; *) rc_file="$$HOME/.profile" ;; esac; \
	fi; \
	chmod 0755 scripts/install-app-layout.sh; \
	chmod 0755 scripts/setup-memory-models.sh; \
	if [ "$(SKIP_MANAGED_COMPONENTS)" = "1" ]; then models_status=skipped; \
	elif [ "$(SKIP_MEMORY_MODELS)" = "1" ]; then LUMINA_APP_ROOT="$(APP_ROOT)" LUMINA_BACKEND_BIN="$(CURDIR)/$(BACKEND_BUILD_PATH)" LUMINA_BGE_METAL_BIN="$(CURDIR)/tmp/lumina-bge-metal" scripts/setup-memory-models.sh doctor; models_status=verified-preinstalled; \
	else LUMINA_APP_ROOT="$(APP_ROOT)" LUMINA_BACKEND_BIN="$(CURDIR)/$(BACKEND_BUILD_PATH)" LUMINA_BGE_METAL_BIN="$(CURDIR)/tmp/lumina-bge-metal" scripts/setup-memory-models.sh install; models_status=installed; fi; \
	NPM="$(NPM)" scripts/install-app-layout.sh "$(APP_ROOT)" "$(CURDIR)/$(BACKEND_BUILD_PATH)" "$(VERSION)"; \
	mkdir -p "$(INSTALL_DIR)"; \
	install -m 0755 "$(BUILD_PATH)" "$(INSTALL_PATH)"; \
	install -m 0755 "$(BACKEND_BUILD_PATH)" "$(BACKEND_INSTALL_PATH)"; \
	if [ "$(SKIP_MANAGED_COMPONENTS)" = "1" ]; then searxng_status=skipped; arxiv_status=skipped; \
	else \
		if LUMINA_APP_ROOT="$(APP_ROOT)" "$(APP_ROOT)/app/scripts/setup-searxng.sh" configure; then searxng_status=configured; else searxng_status=failed; fi; \
		if LUMINA_APP_ROOT="$(APP_ROOT)" "$(APP_ROOT)/app/scripts/setup-arxiv-mcp.sh" install; then arxiv_status=configured; else arxiv_status=failed; fi; \
	fi; \
	if [ "$(INSTALL_DIR)" = "$$HOME/.local/bin" ]; then path_line='export PATH="$$HOME/.local/bin:$$PATH"'; path_marker='$$HOME/.local/bin'; else path_line='export PATH="$(INSTALL_DIR):$$PATH"'; path_marker='$(INSTALL_DIR)'; fi; \
	app_root_line=""; \
	if [ "$(APP_ROOT)" != "$$HOME/.lumina" ]; then app_root_line='export LUMINA_APP_ROOT="$(APP_ROOT)"'; fi; \
	if [ "$(NO_PATH_UPDATE)" != "1" ]; then \
		mkdir -p "$$(dirname "$$rc_file")"; touch "$$rc_file"; \
		if ! printf '%s' "$$PATH" | tr ':' '\n' | grep -Fxqs "$(INSTALL_DIR)" && ! grep -Fqs "$$path_marker" "$$rc_file"; then { printf '\n# LuminaCode CLI\n'; printf '%s\n' "$$path_line"; } >> "$$rc_file"; fi; \
		if [ -n "$$app_root_line" ] && ! grep -Fqs 'LUMINA_APP_ROOT' "$$rc_file"; then { printf '\n# LuminaCode AppRoot\n'; printf '%s\n' "$$app_root_line"; } >> "$$rc_file"; fi; \
	fi; \
	echo "Installed LuminaCode $(VERSION) on $$os_name"; \
	echo "CLI: $(INSTALL_PATH)"; \
	echo "Backend: $(BACKEND_INSTALL_PATH)"; \
	echo "AppRoot: $(APP_ROOT)"; \
	echo "SearxNG: $$searxng_status; arXiv MCP: $$arxiv_status; memory models: $$models_status"; \
	if [ "$(NO_PATH_UPDATE)" != "1" ] && ! command -v "$(APP_NAME)" >/dev/null 2>&1; then echo "Open a new shell or run: source $$rc_file"; fi

doctor:
	@set -eu; \
	backend="$(BACKEND_INSTALL_PATH)"; \
	if [ ! -x "$$backend" ]; then backend="$(BACKEND_BUILD_PATH)"; fi; \
	if [ ! -x "$$backend" ]; then echo "lumina-backend is not installed or built"; exit 1; fi; \
	LUMINA_APP_ROOT="$(APP_ROOT)" "$$backend" layout doctor; \
	if [ -x "$(APP_ROOT)/app/scripts/setup-arxiv-mcp.sh" ]; then LUMINA_APP_ROOT="$(APP_ROOT)" "$(APP_ROOT)/app/scripts/setup-arxiv-mcp.sh" status; fi; \
	if [ -x "$(APP_ROOT)/app/scripts/setup-memory-models.sh" ]; then LUMINA_APP_ROOT="$(APP_ROOT)" LUMINA_BACKEND_BIN="$$backend" "$(APP_ROOT)/app/scripts/setup-memory-models.sh" doctor; fi

uninstall:
	@set -eu; \
	case "$(APP_ROOT)" in /*) ;; *) echo "APP_ROOT must be absolute: $(APP_ROOT)"; exit 1 ;; esac; \
	if [ "$(APP_ROOT)" = "/" ]; then echo "Refusing unsafe APP_ROOT: $(APP_ROOT)"; exit 1; fi; \
	if [ -x "$(BACKEND_INSTALL_PATH)" ]; then LUMINA_APP_ROOT="$(APP_ROOT)" "$(BACKEND_INSTALL_PATH)" shutdown >/dev/null 2>&1 || true; fi; \
	if [ -x "$(APP_ROOT)/app/scripts/setup-arxiv-mcp.sh" ]; then LUMINA_APP_ROOT="$(APP_ROOT)" "$(APP_ROOT)/app/scripts/setup-arxiv-mcp.sh" uninstall || true; elif [ -x scripts/setup-arxiv-mcp.sh ]; then LUMINA_APP_ROOT="$(APP_ROOT)" scripts/setup-arxiv-mcp.sh uninstall || true; fi; \
	if [ -x "$(APP_ROOT)/app/scripts/setup-searxng.sh" ]; then LUMINA_APP_ROOT="$(APP_ROOT)" "$(APP_ROOT)/app/scripts/setup-searxng.sh" uninstall || true; elif [ -x setup-searxng.sh ]; then LUMINA_APP_ROOT="$(APP_ROOT)" ./setup-searxng.sh uninstall || true; fi; \
	if [ -x "$(APP_ROOT)/app/scripts/setup-memory-models.sh" ]; then LUMINA_APP_ROOT="$(APP_ROOT)" LUMINA_BACKEND_BIN="$(BACKEND_INSTALL_PATH)" "$(APP_ROOT)/app/scripts/setup-memory-models.sh" uninstall || true; elif [ -x scripts/setup-memory-models.sh ]; then LUMINA_APP_ROOT="$(APP_ROOT)" LUMINA_BACKEND_BIN="$(BACKEND_INSTALL_PATH)" scripts/setup-memory-models.sh uninstall || true; fi; \
	rm -f "$(INSTALL_PATH)" "$(BACKEND_INSTALL_PATH)"; \
	if [ "$(PURGE)" = "1" ]; then rm -rf "$(APP_ROOT)"; echo "Purged $(APP_ROOT)"; \
	else rm -rf "$(APP_ROOT)/app" "$(APP_ROOT)/cache" "$(APP_ROOT)/state"; echo "Preserved $(APP_ROOT)/config, $(APP_ROOT)/data, and layout.json"; fi; \
	echo "Uninstall complete. Shell rc PATH entries were not modified."

purge:
	@$(MAKE) uninstall PURGE=1

clean:
	@rm -rf "$(BUILD_PATH)" "$(BACKEND_BUILD_PATH)" frontend/dist
