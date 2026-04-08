#!/bin/bash

mkdir -p ../api-server/dist

# copy the matching wasm_exec.js from the tinygo installation to the api-server dist folder, so it can be served to the client
cp /usr/local/lib/tinygo/targets/wasm_exec.js ../api-server/dist/wasm_exec.js

# produce the WASM binary, targeting wasm arch, but with speed optimizations and removing debug info, since this will be used in a performance-critical rendering path
tinygo build -o ./renderer.wasm -target=wasm -no-debug -opt=2 main.go

# super aggressive optimization with wasm-opt, which can significantly reduce the size of the WASM binary and improve performance, at the cost of longer compilation time and potentially less readable code (which is fine for our use case)
wasm-opt -O4 --ignore-implicit-traps --converge --vacuum --flatten -o ../api-server/dist/renderer.wasm renderer.wasm 
