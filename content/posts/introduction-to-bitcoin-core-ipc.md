---
title: Introduction to Bitcoin Core IPC
date: 2026-06-17
toc: true
---

For the last few months I've been studying and contributing to Bitcoin Core's inter-process communication (IPC) layer. I've added test coverage, type mapping support, and logging to the library behind it, and reviewed other contributors' PRs along the way. This post is a breakdown of what the IPC layer is and why it matters.

## The problem with the current architecture

Bitcoin Core today ships as a monolith. `bitcoind` bundles consensus, networking, and wallet into a single binary. `bitcoin-qt` adds the GUI on top. It works, but all those concerns share the same process.

The [process separation project](https://github.com/orgs/bitcoin/projects/8) aims to break this apart into three specialized binaries:

<div class="with-aside">

- `bitcoin-node`: P2P node, indexes, and JSON-RPC server.
- `bitcoin-wallet`: all wallet functionality.
- `bitcoin-gui`: standalone Qt-based GUI.

<aside class="side">
 <p>The three-binary split is the current target, but the idea is to go further, e.g. eventually running indexes in their own process.</p>
</aside>

</div>

![Bitcoin process separation](/images/bitcoin-process-separation.png)

Why break apart a binary that already works?

**Security.** Smaller attack surface. In a monolith, the same `bitcoind` process that parses P2P messages also holds the wallet keys, so a memory bug in that parsing code, like an out-of-bounds read in message deserialization, could read the wallet keys straight off the heap. Split into `bitcoin-node` and `bitcoin-wallet`, that same read sees only the `bitcoin-node`'s memory and not `bitcoin-wallet`'s. Crashes also stay contained: if `bitcoin-gui` goes down, `bitcoin-node` keeps running, whereas in a monolith a crash anywhere takes the whole process down.

<div class="with-aside">

**Usability.** You can start the wallet and/or the GUI only when you need it instead of keeping them running all the time.

<aside class="side">
  <p>Eventually the processes won't even have to live on the same machine: once TCP support lands, you could run <code>bitcoin-node</code> on a remote server and connect <code>bitcoin-wallet</code> and <code>bitcoin-gui</code> from your laptop.</p>
</aside>

</div>

**Maintenance.** Splitting into binaries forces clear interface boundaries between what each process owns, which makes the code easier to reason about.

But splitting the monolith breaks something that used to be free. In `bitcoind`, the wallet and the node live in the same process: they share memory and call each other's functions directly. The wallet still needs to ask the node for blocks, and the node still needs to notify the wallet about new transactions, but once they are separate processes a function call in one can no longer reach a pointer in another. They need a way to talk across the process boundary, and that mechanism is IPC.

IPC restores the conversation, but not for free. A call that used to be a plain function call now has to copy its arguments into a message and pass them across the process boundary, which adds some overhead, and debugging gets harder once a single logical operation spans two processes.

## What is IPC?

<div class="with-aside">

IPC (inter-process communication) is how separate processes talk to each other. The OS provides several mechanisms for this: shared memory, Unix domain sockets, pipes. They differ in performance, complexity, portability, and semantics.

<aside class="side">
  <p>For more details about IPC mechanisms I recommend <a href="https://dev.to/cear/inter-process-communication-ipc-in-c-complete-guide-56mg">this blog post</a>.</p>
</aside>

</div>

Bitcoin Core currently uses Unix domain sockets for local communication between processes. TCP sockets are going to be used to [support Windows](https://github.com/bitcoin/bitcoin/pull/32387), and could eventually let processes running on different machines talk to each other.

## How does Bitcoin Core IPC work?

Bitcoin Core relies on [libmultiprocess](https://github.com/bitcoin-core/libmultiprocess), a C++ library and code generator that makes calling functions and referencing objects across process boundaries feel like ordinary C++ code. It uses [Cap'n Proto](https://capnproto.org/) as its RPC framework over Unix domain sockets. Cap'n Proto [was chosen](https://github.com/bitcoin/bitcoin/blob/master/doc/design/multiprocess.md) over alternatives like gRPC or JSON-RPC because it allows interface pointers to be passed as method arguments and return values, so object references and bidirectional requests work out of the box. That matches how Bitcoin Core's internal interfaces are structured.

## Inside libmultiprocess

libmultiprocess does its work in two phases. At build time, a code generator writes the glue code. At run time, a runtime carries the calls.

**mpgen** is a code generator that reads `.capnp` interface definition files and produces C++ client and server classes. Here's a trimmed piece of a real one, [`mining.capnp`](https://github.com/bitcoin/bitcoin/blob/master/src/ipc/capnp/mining.capnp), the interface mining software uses to talk to the node:

```capnp
interface Mining $Proxy.wrap("interfaces::Mining") {
    isTestChain @0 (context :Proxy.Context) -> (result: Bool);
    createNewBlock @4 (context :Proxy.Context,
                       options: BlockCreateOptions)
                      -> (result: BlockTemplate);
}
```

Note the return type of `createNewBlock`: `BlockTemplate` is not a struct, it's another interface, and that distinction is the whole point. A struct return value gets serialized and copied across the socket. An interface return value does not: the `BlockTemplate` is constructed inside the node and never leaves it. What actually crosses the wire is a capability id, a small handle pointing back to that one object. The caller holds a proxy that looks like a local `BlockTemplate`, but every method it calls (`getBlock`, `submitSolution`) is routed back over the socket to the real object living in the node. This is the interface-pointers-as-arguments capability that drove the choice of Cap'n Proto: references to objects, not just plain data, can be passed as arguments and return values.

From a file like this, mpgen produces C++ client and server classes. The generated client class looks like a normal C++ object from the caller's side. The `$Proxy.wrap` annotation ties it to the existing `interfaces::Mining` abstract class, so callers don't even see a new type. The generated server class receives the call on the other end and dispatches it to the real implementation. All socket handling is hidden inside the generated code.

**The runtime** manages connections, threading, and serialization at call time. When you call a method on an IPC object, the runtime serializes the arguments over the socket, waits for the response, and deserializes the return value. Serialization here means mapping C++ types to Cap'n Proto structures and back. I've [worked on that mapping](https://github.com/bitcoin-core/libmultiprocess/pull/277), upstreaming `std::unordered_set` support from Bitcoin Core into libmultiprocess, and it works exactly how you'd guess: the runtime knows how to build and read the wire representation of each supported type.

<div class="with-aside">

From the caller's perspective it behaves like a normal function call. Nothing in the calling code hints that the call traveled to another process and came back. That's the magic: the implementation code doesn't need to change at all to work across process boundaries. The separation is entirely handled by the generated glue and the runtime beneath it.

<aside class="side">
  <p>libmultiprocess is only a convenience for C++ code. On the wire it's plain Cap'n Proto, so clients written in any language can talk to the node without it.</p>
</aside>

</div>

The threading part is easy to underestimate. It's not just a socket: all traffic routes through a dedicated event loop thread, and each thread making IPC calls gets a matching thread on the other side. The main reason is deadlock prevention.

Picture a thread that already holds a lock making a synchronous IPC call, and the other side calling back into it while the call is in flight. Bitcoin Core leans on recursive mutexes like `cs_main`: in a single process the callback runs on the thread that holds the lock, so it re-enters and proceeds. Across processes, if that callback landed on an arbitrary thread, it would block trying to take a lock the original thread still holds, and the call would deadlock. That's why each request carries a `callbackThread` reference: callbacks are routed back to the exact thread that made the original call, so recursive mutexes re-enter as expected and thread names stay meaningful, just like in the single-process code.

## Current status

The full process separation vision is still in development. As of Bitcoin Core v31.0, multiprocess mode runs the same monolithic node code, just with an IPC socket exposed so external clients can connect to it. The true internal split between `bitcoin-node`, `bitcoin-wallet`, and `bitcoin-gui` is not yet released.

The internal split is still landing in PRs. The full set of open work and the current state of the project is tracked in [issue #28722](https://github.com/bitcoin/bitcoin/issues/28722). The base PR to follow is [#10102](https://github.com/bitcoin/bitcoin/pull/10102), which lets you build `bitcoin-node`, `bitcoin-wallet`, and `bitcoin-gui` as separate binaries connected over IPC. Once built, running `bitcoin -m node -debug=ipc` starts `bitcoin-node`, which in turn starts `bitcoin-wallet`, and the two talk to each other over IPC. With `-debug=ipc` you can watch those messages go back and forth in the logs (the line announcing a new socket connection was [my first contribution](https://github.com/bitcoin-core/libmultiprocess/pull/258) to libmultiprocess).

The first interface exposed for external use is mining. Core v30.0 shipped an experimental IPC mining interface, a generic, typed alternative to the mining JSON-RPC flow, designed so a [Stratum V2](https://stratumprotocol.org/) or other mining client software can fetch templates and submit solutions over IPC. The v31.0 release improved this mining interface, though it is still experimental.

This post stayed at the high level on purpose. In the next one I'll get on the ground: running `bitcoin-node` and `bitcoin-wallet` and tracing the actual Cap'n Proto messages between them.
