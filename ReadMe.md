# Segmentation Renderer
This component tries to aggressively accelerate the computational speed of COCO RLE buffers such we can improve system 
performance, and reduce the reliance on caching canvases which at scale will lead to performance problems, or cause OOM if 
the browser hits a 4GiB cap. 

https://www.electronjs.org/blog/v8-memory-cage

WASM allows us to get near-system level performance in a way that is safe and secure within the browser runtime. 

## Links:

Tinygo for better stripped compilation for WASM: https://tinygo.org/docs/guides/webassembly/wasm/

**NOTE**: We don't intend to render in the same stage as retrieval. But we should trial various stages of closeness to the final version. 
          The less we cache, the more likely it is that this application can scale with larger videos, something that would add greater 
          value, as less segments means less overall clicking.

Memory64 is a proposal that would allow the V8 cap to be lifted up to 16GiB of memory in the future, so eventual binary builds as "wasm64" will be possible. Making it easier to perform caching for perf optimization.

This application is imported by VideoSelection.tsx while the SAM2 tool is being used.

# Dependencies
tinygo ->  (used in optimization and compression of output side)
binaryen ->  (used in aggressive speed enhancements)

```bash
sudo apt-get install binaryen
```