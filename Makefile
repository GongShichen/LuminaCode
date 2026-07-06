APP_NAME ?= lumina
GO ?= go
BUILD_DIR ?= tmp
INSTALL_DIR ?= $(shell os=$$(uname -s); if [ "$$os" = "Darwin" ] && [ -d /opt/homebrew/bin ] && [ -w /opt/homebrew/bin ]; then printf /opt/homebrew/bin; elif [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then printf /usr/local/bin; else printf '%s/.local/bin' "$$HOME"; fi)
APP_ROOT ?= $(HOME)/.lumina
BUILD_PATH := $(BUILD_DIR)/$(APP_NAME)
INSTALL_PATH := $(INSTALL_DIR)/$(APP_NAME)

.PHONY: help build install uninstall doctor clean

help:
	@printf '%s\n' \
		'LuminaCode Makefile' \
		'' \
		'Targets:' \
		'  make build      Build ./tmp/lumina' \
		'  make install    Build lumina, install it into a PATH bin dir, and copy resources into ~/.lumina' \
		'  make doctor     Show detected OS, shell, rc file, and install path' \
		'  make uninstall  Remove the installed lumina binary' \
		'  make clean      Remove local build output' \
		'' \
		'Default install dir prefers writable /opt/homebrew/bin or /usr/local/bin, then ~/.local/bin.' \
		'' \
		'Overrides:' \
		'  make install INSTALL_DIR=/usr/local/bin' \
		'  make install APP_ROOT=/opt/lumina'

build:
	@mkdir -p "$(BUILD_DIR)"
	$(GO) build -o "$(BUILD_PATH)" .

install: build
	@set -eu; \
	os="$$(uname -s)"; \
	case "$$os" in \
		Darwin) os_name="macOS" ;; \
		Linux) os_name="Linux" ;; \
		*) echo "Unsupported OS: $$os"; exit 1 ;; \
	esac; \
	shell_path=""; \
	if [ "$$os" = "Darwin" ]; then \
		shell_path="$$(dscl . -read "/Users/$$(id -un)" UserShell 2>/dev/null | awk '{print $$2}' || true)"; \
	else \
		shell_path="$$(getent passwd "$$(id -un)" 2>/dev/null | cut -d: -f7 || true)"; \
		if [ -z "$$shell_path" ]; then \
			shell_path="$$(awk -F: -v user="$$(id -un)" '$$1 == user {print $$7}' /etc/passwd 2>/dev/null || true)"; \
		fi; \
	fi; \
	if [ -z "$$shell_path" ]; then \
		shell_path="$${SHELL:-/bin/sh}"; \
	fi; \
	shell_name="$$(basename "$$shell_path")"; \
	case "$$shell_name" in \
		zsh) rc_file="$${ZDOTDIR:-$$HOME}/.zshrc" ;; \
		bash) \
			if [ "$$os" = "Darwin" ]; then \
				rc_file="$$HOME/.bash_profile"; \
			else \
				rc_file="$$HOME/.bashrc"; \
			fi ;; \
		*) \
			rc_file="$$HOME/.profile"; \
			echo "Unsupported login shell '$$shell_name'; using $$rc_file for PATH setup." ;; \
	esac; \
	mkdir -p "$(INSTALL_DIR)"; \
	install -m 0755 "$(BUILD_PATH)" "$(INSTALL_PATH)"; \
	if [ -z "$(APP_ROOT)" ] || [ "$(APP_ROOT)" = "/" ]; then \
		echo "Refusing unsafe APP_ROOT: $(APP_ROOT)"; \
		exit 1; \
	fi; \
	mkdir -p "$(APP_ROOT)"; \
	preserved_config=""; \
	if [ -f "$(APP_ROOT)/CONFIG/defaults.json" ]; then \
		preserved_config="$$(mktemp)"; \
		cp "$(APP_ROOT)/CONFIG/defaults.json" "$$preserved_config"; \
	fi; \
	cp -R ".Lumina/." "$(APP_ROOT)/"; \
	if [ -n "$$preserved_config" ]; then \
		mkdir -p "$(APP_ROOT)/CONFIG"; \
		cp "$$preserved_config" "$(APP_ROOT)/CONFIG/defaults.json"; \
		rm -f "$$preserved_config"; \
	fi; \
	if [ "$(INSTALL_DIR)" = "$$HOME/.local/bin" ]; then \
		path_line='export PATH="$$HOME/.local/bin:$$PATH"'; \
		path_marker='$$HOME/.local/bin'; \
	else \
		path_line='export PATH="$(INSTALL_DIR):$$PATH"'; \
		path_marker='$(INSTALL_DIR)'; \
	fi; \
	resource_line=""; \
	if [ "$(APP_ROOT)" != "$$HOME/.lumina" ]; then \
		resource_line='export LUMINA_RESOURCE_ROOT="$(APP_ROOT)"'; \
	fi; \
	added_path=0; \
	added_resource_root=0; \
	mkdir -p "$$(dirname "$$rc_file")"; \
	touch "$$rc_file"; \
	if ! printf '%s' "$$PATH" | tr ':' '\n' | grep -Fxqs "$(INSTALL_DIR)" && ! grep -Fqs "$$path_marker" "$$rc_file" && ! grep -Fqs "$(INSTALL_DIR)" "$$rc_file"; then \
		{ printf '\n# LuminaCode CLI\n'; printf '%s\n' "$$path_line"; } >> "$$rc_file"; \
		added_path=1; \
	fi; \
	if [ -n "$$resource_line" ] && ! grep -Fqs "LUMINA_RESOURCE_ROOT" "$$rc_file"; then \
		{ printf '\n# LuminaCode resources\n'; printf '%s\n' "$$resource_line"; } >> "$$rc_file"; \
		added_resource_root=1; \
	fi; \
	if [ "$(INSTALL_PATH)" != "$$HOME/.local/bin/$(APP_NAME)" ]; then \
		rm -f "$$HOME/.local/bin/$(APP_NAME)"; \
	fi; \
	echo "Installed $(APP_NAME) to $(INSTALL_PATH)"; \
	echo "Installed resources to $(APP_ROOT)"; \
	if [ -n "$$preserved_config" ]; then \
		echo "Preserved existing $(APP_ROOT)/CONFIG/defaults.json"; \
	fi; \
	echo "Detected $$os_name with $$shell_name ($$shell_path)"; \
	if [ "$$added_path" = "1" ]; then \
		echo "Updated PATH in $$rc_file"; \
	elif printf '%s' "$$PATH" | tr ':' '\n' | grep -Fxqs "$(INSTALL_DIR)"; then \
		echo "$(INSTALL_DIR) is already in current PATH"; \
	else \
		echo "PATH entry already exists in $$rc_file"; \
	fi; \
	if [ "$$added_resource_root" = "1" ]; then \
		echo "Updated LUMINA_RESOURCE_ROOT in $$rc_file"; \
	elif [ -n "$$resource_line" ]; then \
		echo "LUMINA_RESOURCE_ROOT already exists in $$rc_file"; \
	else \
		echo "Default resource root does not require LUMINA_RESOURCE_ROOT"; \
	fi; \
	if command -v "$(APP_NAME)" >/dev/null 2>&1; then \
		echo "Ready: $$(command -v "$(APP_NAME)")"; \
	elif [ "$$added_path" = "1" ] || [ "$$added_resource_root" = "1" ]; then \
		echo "Run: source $$rc_file"; \
	fi

doctor:
	@set -eu; \
	os="$$(uname -s)"; \
	case "$$os" in \
		Darwin) os_name="macOS" ;; \
		Linux) os_name="Linux" ;; \
		*) os_name="unsupported ($$os)" ;; \
	esac; \
	shell_path=""; \
	if [ "$$os" = "Darwin" ]; then \
		shell_path="$$(dscl . -read "/Users/$$(id -un)" UserShell 2>/dev/null | awk '{print $$2}' || true)"; \
	elif [ "$$os" = "Linux" ]; then \
		shell_path="$$(getent passwd "$$(id -un)" 2>/dev/null | cut -d: -f7 || true)"; \
		if [ -z "$$shell_path" ]; then \
			shell_path="$$(awk -F: -v user="$$(id -un)" '$$1 == user {print $$7}' /etc/passwd 2>/dev/null || true)"; \
		fi; \
	fi; \
	if [ -z "$$shell_path" ]; then \
		shell_path="$${SHELL:-/bin/sh}"; \
	fi; \
	shell_name="$$(basename "$$shell_path")"; \
	case "$$shell_name" in \
		zsh) rc_file="$${ZDOTDIR:-$$HOME}/.zshrc" ;; \
		bash) \
			if [ "$$os" = "Darwin" ]; then \
				rc_file="$$HOME/.bash_profile"; \
			else \
				rc_file="$$HOME/.bashrc"; \
			fi ;; \
		*) rc_file="$$HOME/.profile" ;; \
	esac; \
	printf 'OS:           %s\n' "$$os_name"; \
	printf 'Login shell:  %s\n' "$$shell_path"; \
	printf 'Shell type:   %s\n' "$$shell_name"; \
	printf 'RC file:      %s\n' "$$rc_file"; \
	printf 'Install path: %s\n' "$(INSTALL_PATH)"; \
	printf 'Resource root:%s\n' " $(APP_ROOT)"; \
	if printf '%s' "$$PATH" | tr ':' '\n' | grep -Fxqs "$(INSTALL_DIR)"; then \
		printf 'PATH status:  install dir is in current PATH\n'; \
	else \
		printf 'PATH status:  install dir is not in current PATH\n'; \
	fi; \
	if [ -d "$(APP_ROOT)/CONFIG" ] && [ -d "$(APP_ROOT)/SYSTEM" ] && [ -d "$(APP_ROOT)/SKILLS" ]; then \
		printf 'Resources:    installed\n'; \
	else \
		printf 'Resources:    not installed\n'; \
	fi; \
	if command -v "$(APP_NAME)" >/dev/null 2>&1; then \
		printf 'Command:      %s\n' "$$(command -v "$(APP_NAME)")"; \
	else \
		printf 'Command:      not found in current PATH\n'; \
	fi

uninstall:
	@rm -f "$(INSTALL_PATH)"
	@if [ "$(INSTALL_PATH)" != "$(HOME)/.local/bin/$(APP_NAME)" ]; then \
		rm -f "$(HOME)/.local/bin/$(APP_NAME)"; \
	fi
	@if [ -z "$(APP_ROOT)" ] || [ "$(APP_ROOT)" = "/" ]; then \
		echo "Refusing unsafe APP_ROOT: $(APP_ROOT)"; \
		exit 1; \
	fi
	@rm -rf "$(APP_ROOT)"
	@echo "Removed $(INSTALL_PATH)"
	@echo "Removed $(APP_ROOT)"
	@echo "PATH lines in shell rc files are left untouched."

clean:
	@rm -rf "$(BUILD_DIR)/$(APP_NAME)"
