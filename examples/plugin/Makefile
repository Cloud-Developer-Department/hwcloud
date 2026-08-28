.PHONY: all tool observer envsync clean

PLUGINS_DIR := ./plugins

all: tool observer envsync

tool:
	mkdir -p $(PLUGINS_DIR)
	cd tool && cargo +stable build --release --target wasm32-unknown-unknown
	cp tool/target/wasm32-unknown-unknown/release/example_agent_tool.wasm $(PLUGINS_DIR)/echo.wasm

observer:
	mkdir -p $(PLUGINS_DIR)
	cd observer && cargo +stable build --release --target wasm32-unknown-unknown
	cp observer/target/wasm32-unknown-unknown/release/example_agent_observer.wasm $(PLUGINS_DIR)/logger.wasm

envsync:
	mkdir -p $(PLUGINS_DIR)
	cd envsync && cargo +stable build --release --target wasm32-unknown-unknown
	cp envsync/target/wasm32-unknown-unknown/release/example_agent_envsync.wasm $(PLUGINS_DIR)/envsync.wasm

clean:
	rm -f $(PLUGINS_DIR)/*.wasm
	cd tool && cargo clean
	cd observer && cargo clean
	cd envsync && cargo clean
