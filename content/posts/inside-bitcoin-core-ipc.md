---
title: Inside Bitcoin Core IPC
date: 2026-07-23
toc: true
---

<nav class="series">
  <p>Part 1 of 3 on Bitcoin Core IPC. Background: <a href="/posts/introduction-to-bitcoin-core-ipc">Introduction to Bitcoin Core IPC</a>.</p>
  <ol>
    <li aria-current="page">Inside Bitcoin Core IPC</li>
    <li><a href="/posts/bitcoin-core-ipc-event-loop">Bitcoin Core IPC Event Loop</a></li>
    <li><a href="/posts/bitcoin-core-ipc-on-the-wire">Bitcoin Core IPC on the Wire</a></li>
  </ol>
</nav>

In [my last post](https://viniciuscestari.dev/posts/introduction-to-bitcoin-core-ipc) I made an introduction to the Bitcoin Core IPC mechanism and why it is important. In this one I want to dive deeper and show this mechanism working in practice between `bitcoin-node` and `bitcoin-wallet` binaries.

## Setting Up

<div class="with-aside">

We will need to use the changes on the [PR #10102](https://github.com/bitcoin/bitcoin/pull/10102) named Multiprocess bitcoin to try the split of `bitcoin-node` and `bitcoin-wallet`.

<aside class="side">
  <p>Fun fact: <a href="https://github.com/bitcoin/bitcoin/pull/10102">PR #10102</a> is the oldest open PR in Bitcoin Core!</p>
</aside>

</div>

Since this PR is still open and being developed, the code here may change, but the core idea and flow should stay the same. The links here and my review are based on the HEAD of [PR #10102](<https://github.com/bitcoin/bitcoin/pull/10102>) at the time of writing ([b314fb2](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337)).

### Building Bitcoin Core IPC

<div class="with-aside">

Building it is pretty straightforward because the default build already includes the multiprocess binaries.

```bash
cmake -B build
cmake --build build -j $(nproc)
```

This may take a while but after running this you should see some binaries in the folder `./build/bin/`, including `bitcoin`, `bitcoin-node` and `bitcoin-wallet`.

<aside class="side">
  <p><a href="https://github.com/bitcoin/bitcoin/pull/32387">Windows support</a> for this build is still in development.</p>
</aside>

</div>

## Running Multiprocess Bitcoin Core

<div class="with-aside">

Using the `bitcoin` CLI we can start `bitcoin-node`:

```bash
build/bin/bitcoin -m node -debug=ipc -regtest
```

Here we use the `-m` flag to state that we want to run the multiprocess `bitcoin-node` and not `bitcoind`, `-debug=ipc` to print IPC logs, and `-regtest` to run on the [regtest network](https://developer.bitcoin.org/examples/testing.html#regtest-mode). 

<aside class="side">
  <p><code>bitcoin</code> is just a CLI helper to run other commands. To see more, check <a href="https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/bitcoin.cpp">src/bitcoin.cpp</a>.</p>
</aside>

</div>


Ok, now with `bitcoin-node` running, let's run `bitcoin-wallet`. But wait, let's first run this command to see our `bitcoin-node` running:

<div class="with-aside">

```bash
vinicius@archlinux ~> pstree | grep bitcoin
        |         |-alacritty-+-fish---bitcoin-node-+-bitcoin-wallet---2*[{bitcoin-wallet}]
        |         |           |                     `-48*[{bitcoin-node}]
```

<aside class="side">
  <p>The <code>*</code> numbers are just the number of OS threads of that process.</p>
</aside>

</div>

What does this mean? Well, in my case I ran the terminal emulator [Alacritty](https://alacritty.org/), which runs the [fish shell](https://fishshell.com/), and there I ran `build/bin/bitcoin -m node -debug=ipc -regtest`. Then [`bitcoin` called `execvp` with `./build/bin/bitcoin-node`](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/bitcoin.cpp#L203), replacing the `bitcoin` process image with a `bitcoin-node` process image (that's why we don't see a `bitcoin` binary in the command output). 

Now the interesting part: `bitcoin-node` spawned `bitcoin-wallet`. This preserves monolith-equivalent UX (`bitcoin-node` "just runs a wallet" like `bitcoind` does) without you manually starting and wiring up `bitcoin-wallet` yourself.

We can also look at the PID (process id) and full command of each process:

```bash
vinicius@archlinux ~> ps a -o pid,cmd | grep bitcoin
  71906 /home/vinicius/Code/my/bitcoin/build/bin/bitcoin-node -debug=ipc -regtest
  71908 /home/vinicius/Code/my/bitcoin/build/bin/bitcoin-wallet -ipcfd 8
```

`bitcoin-node` got the args we passed to `bitcoin`, as expected. But `bitcoin-wallet` got an arg we never typed: `-ipcfd 8`.

That 8 is a [file descriptor](https://pubs.opengroup.org/onlinepubs/9799919799/basedefs/V1_chap03.html#tag_03_141). Before spawning the child, `bitcoin-node` creates a [Unix Domain socket](https://man7.org/linux/man-pages/man7/unix.7.html) and keeps one end for itself. The other end stays open in the child (in this case fd 8), and `-ipcfd` is how the child gets told which fd to use to talk to the parent.

`bitcoin-wallet` was originally an offline wallet tool: a CLI you run to create a wallet or dump its contents. That is still what it does by default. But when it starts it also checks for `-ipcfd`, and if the flag is set it never runs a CLI command at all: [it turns into an IPC server](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/interfaces.cpp#L80) that waits on that fd for `bitcoin-node` to call it.

It might sound weird at first that the same binary behaves completely differently depending on whether it got `-ipcfd`. The [reasoning](https://github.com/bitcoin/bitcoin/issues/31827#issuecomment-2648684591) is that you are not supposed to call `bitcoin-wallet` yourself: you call the `bitcoin` wrapper, and it decides which binary to run and how. Which binary ends up serving your command is an implementation detail.

## What to Expect on the Wire

Before opening a single trace file, it is worth knowing what we are looking for. Otherwise the bytes are just bytes.

What we are looking for is one call: `Init.makeWalletLoader`. A wallet loader is the object that owns wallet files, the thing that creates them, loads them, lists them, and hands back a `Wallet` for each one. It is what `bitcoin-node` has to obtain before it can do anything with a wallet at all.

<div class="with-aside">

I picked it because it is the first message on this socket that a Bitcoin Core developer would recognize, which also makes it the right place to stop. Everything before it is capnp and libmultiprocess getting the connection into a usable state, machinery that does not exist in `bitcoind` and does not correspond to anything in the node's own logic, and everything after it is a connection that already works. `makeWalletLoader` is the one point where the trace shows something that is not setup: in `bitcoind` it is a plain C++ call on a pointer, one line, no socket involved, and here that same line is a message we can follow from the C++ that made it down to the bytes.

<aside class="side">
  <p>libmultiprocess is the library that generates the C++ proxy classes code at build time, and drives the connection the processes talk over at run time. I explain it <a href="/posts/introduction-to-bitcoin-core-ipc#how-does-bitcoin-core-ipc-work">here</a></p>
</aside>

</div>


### capnp schema

Everything the two processes say to each other is described, ahead of time, by a set of schema files that live in [`src/ipc/capnp/`](https://github.com/bitcoin/bitcoin/tree/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/capnp). They are written in the [Cap'n Proto schema language](https://capnproto.org/language.html), and each binary is compiled against them, so there is no negotiation at runtime: the vocabulary is fixed at build time.

The most important interface is [`Init`](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/capnp/init.capnp):

```capnp
@0xf2c5cfa319406aa6;

interface Init $Proxy.wrap("interfaces::Init") {
    construct @0 (threadMap: Proxy.ThreadMap) -> (threadMap :Proxy.ThreadMap);
    makeEcho @1 (context :Proxy.Context) -> (result :Echo.Echo);
    makeMining @3 (context :Proxy.Context) -> (result :Mining.Mining);
    makeRpc @4 (context :Proxy.Context) -> (result :Rpc.Rpc);
    makeChain @5 (context :Proxy.Context) -> (result :Chain.Chain);
    makeNode @6 (context :Proxy.Context) -> (result :Node.Node);
    makeWalletLoader @7 (context :Proxy.Context, globalArgs :Common.GlobalArgs, chain :Chain.Chain) -> (result :Wallet.WalletLoader);

    # DEPRECATED: no longer supported; server returns an error.
    makeMiningOld2 @2 () -> ();
}
```

<div class="with-aside">

An `interface` is capnp's word for an object you can call methods on remotely. It is never copied on the wire, it stays where it is and you get the ability to call it.

<aside class="side">
  <p>Compare with <a href="https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/capnp/common.capnp">common.capnp</a>, which is almost all <code>struct</code>: those are the things that really do travel as bytes.</p>
</aside>

</div>

Now look at the numbers. Every method carries an ordinal, `@0` through `@7`, and those ordinals are what actually go on the wire. Method names are for us, not for the protocol. Same for `@0xf2c5cfa319406aa6` at the top of the file: capnp derives a 64 bit id for every type from the file id and the type name, and *that* is how the receiving side knows which interface you meant. So the pair we should expect to see travelling is an interface id plus a method number, not the string `makeWalletLoader`.

You can also see why `makeMiningOld2 @2` is still sitting there, deprecated and out of order. Deleting it would let `@2` be reused by a future method, and then an old peer calling `@2` would silently invoke something else. [The schema language rules](https://capnproto.org/language.html#evolving-your-protocol) are explicit about this: you may add, you may rename, you must never renumber.

The `$Proxy.wrap("interfaces::Init")` bit is not for capnp, it is an [annotation](https://capnproto.org/language.html#annotations) that libmultiprocess defines in [`mp/proxy.capnp`](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/libmultiprocess/include/mp/proxy.capnp) and its code generator tool, `mpgen`, reads it to generate code. It says: this capnp interface mirrors the C++ class `interfaces::Init`, so generate a `ProxyClient<Init>` that inherits from it and turns each method call into a request, and a `ProxyServer<Init>` that unpacks requests and calls the real object.And the `make*` methods really do mirror it one for one, [`src/interfaces/init.h`](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/interfaces/init.h#L31):

```c++
class Init
{
public:
    virtual ~Init() = default;
    virtual std::unique_ptr<Node> makeNode() { return nullptr; }
    virtual std::unique_ptr<Chain> makeChain() { return nullptr; }
    virtual std::unique_ptr<Mining> makeMining() { return nullptr; }
    virtual std::unique_ptr<WalletLoader> makeWalletLoader(Chain& chain) { return nullptr; }
    // ...
};
```

Because the two match method for method, the code that uses `interfaces::Init` does not have to know which one it got. It holds a pointer to the abstract class and calls virtual methods on it. If the implementation behind that pointer is the local one, the call is a normal function call. If it is the generated `ProxyClient<Init>`, the exact same call turns into a request on a socket. Nothing at the call site says "this is IPC", and nothing there had to be rewritten to make it IPC.

That is why `WalletInit::Construct()`, the startup function that asks for a wallet loader in the first place, looks like it is just calling ordinary C++ methods:

```c++
auto wallet_loader = node.init->makeWalletLoader(*node.chain);
```

It is an ordinary C++ method call. `node.init` is just a pointer to an abstract class, and in `bitcoind` that is exactly all it is. In `bitcoin-node` the object behind that pointer is a generated proxy, and this one line becomes everything the rest of this series is about.

### Capabilities

Look at what `Init.makeWalletLoader` returns: `result :Wallet.WalletLoader`. `WalletLoader` is an `interface`, so the reply cannot possibly contain a wallet loader. Wallet loaders are made of open database handles and file locks and threads, none of which fit in a socket. What comes back instead is a *capability*: a reference to a live object over there, plus the right to call its methods.

A capnp connection has room for exactly one starting point, a [bootstrap capability](https://github.com/capnproto/capnproto/blob/v1.2.0/c++/src/capnp/rpc.capnp#L278), and every other capability has to be reached from it. libmultiprocess picks the bootstrap when the connection is made, and for Bitcoin Core it is always `Init`, [in `ConnectStream`](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/libmultiprocess/include/mp/proxy-io.h#L839):

```c++
init_client = connection->m_rpc_system->bootstrap(ServerVatId().vat_id).castAs<InitInterface>();
```

That explains the shape of `Init`. It is nothing but `make*` methods, because it is like a root of a graph: `Init` gets you a `WalletLoader`, `WalletLoader.createWallet` gets you a [`Wallet`](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/capnp/wallet.capnp#L18), and only then can you call `getBalances`, etc. Each interface is a door to the next one.

```capnp
interface WalletLoader extends(Chain.ChainClient) $Proxy.wrap("interfaces::WalletLoader") {
    createWallet @0 (context :Proxy.Context, name :Text, passphrase :Text, flags :UInt64) -> (warning :List(Common.BilingualStr), result :Common.Result(Wallet));
    loadWallet @1 (context :Proxy.Context, name :Text) -> (warning :List(Common.BilingualStr), result :Common.Result(Wallet));
    // ...
    handleLoadWallet @8 (context :Proxy.Context, callback :LoadWalletCallback) -> (result :Handler.Handler);
}
```

Capabilities go the other way too. `handleLoadWallet` *takes* a `LoadWalletCallback`, which is an interface, so `bitcoin-node` is handing `bitcoin-wallet` a reference to an object of its own, and `bitcoin-wallet` will call back into `bitcoin-node` through it. This is why there is no client and no server here. Both processes export capabilities, both call.

Two more conventions generate traffic we would otherwise have to explain twice:

- Almost every method takes `context :Proxy.Context` as its first parameter. That is not a Bitcoin Core parameter, it is [libmultiprocess bookkeeping](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/libmultiprocess/include/mp/proxy.capnp#L60), and it holds two `Thread` capabilities pointing opposite ways: `thread`, the server thread this call should run on, and `callbackThread`, the client thread the server should route callbacks back to, which is how a recursive lock still re-enters across a process boundary. The two are not obtained the same way. `callbackThread` points at an object in the caller's own process, so the caller just creates it and hands out a capability to it. `thread` points at an object in the other process, so it has to be asked for with a `ThreadMap.makeThread` call before it can be named. That happens [once per thread per connection](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/libmultiprocess/include/mp/type-context.h#L24), not once per call, but it does mean the first call from any thread drags an extra one along with it.
- `construct @0` on `Init` and `destroy @0` on most other interfaces are lifecycle hooks. `construct` runs [automatically](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/libmultiprocess/include/mp/proxy-io.h#L590) when the proxy client is built and exists [only to swap `ThreadMap` capabilities](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/libmultiprocess/include/mp/proxy.h#L104) between the two sides. `destroy` exists so that object lifetimes stay tied together across the process boundary: when a proxy goes out of scope on one side, the real object on the other side is gone before the destructor returns. The proxy's destructor sends a `destroy` call over the socket and blocks until the reply comes back, [waiting for the object to actually be deleted server side](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/libmultiprocess/include/mp/proxy-io.h#L558), so `delete` is a round trip like any other method.

### The shape of the traffic

That is enough to predict every message that crosses the socket before `bitcoin-node` holds a wallet loader, without looking at a single byte:

1. **`bitcoin-node` asks for the bootstrap capability.** A capnp connection starts with exactly one object handed over, and everything else is reached through it. Here that object is `Init`.
2. **It sets up the plumbing on that capability.** `Init.construct` runs to swap `ThreadMap` capabilities, and then `ThreadMap.makeThread` runs to create the `bitcoin-wallet` side thread that will serve the call, because `Init.makeWalletLoader` takes a `context :Proxy.Context`, and `context.thread` names a thread in `bitcoin-wallet`, which has to exist before it can be named. Its sibling `context.callbackThread` costs no message at all, since that thread is `bitcoin-node`'s own, and handing out a capability to an object you already have is free.
3. **Then the call we want to see: `Init.makeWalletLoader`**, naming the `Init` interface id and method `7`. `globalArgs` travels as copied bytes, since it is a struct. `context.thread`, `context.callbackThread` and `chain` travel as capabilities, since they are interfaces. `chain` is the one worth noticing: it is `bitcoin-node`'s own `Chain` object, so this is also the call where `bitcoin-wallet` ends up holding a capability that points back into `bitcoin-node`.
4. **And a return that contains only a wallet loader capability.** Not an object that is made of file locks and threads copied into the wire, but only a capability pointing at one.

Cap'n Proto is two things: a serialization format, and an RPC protocol built on top of it. Everything above is the serialization format carrying Bitcoin Core's own schemas. The envelope around all of it is the RPC protocol, and it is described by a capnp schema too, [`rpc.capnp`](https://github.com/capnproto/capnproto/blob/v1.2.0/c%2B%2B/src/capnp/rpc.capnp): a `Call` carrying a `questionId`, a `target`, an `interfaceId`, a `methodId` and `params`, answered by a `Return` carrying the matching `answerId`.

<div class="with-aside">

All of it came from schema files and `pstree`, without tracing anything. Everything from here on is checking the prediction against reality.

<aside class="side">
  <p><strong>Spoiler:</strong> the prediction holds, but not as simple and straightfoward as the list suggests. The wire is much more interesting, and faster.</p>
</aside>

</div>

That takes two more posts. In the [next one](https://viniciuscestari.dev/posts/bitcoin-core-ipc-event-loop) we put `strace` on both processes and follow the machinery underneath these four steps: which file descriptor each side ends up holding, and which thread actually writes the bytes. In the [one after that](https://viniciuscestari.dev/posts/bitcoin-core-ipc-on-the-wire) we decode the bytes themselves and find out how a capability is spelled on the wire, and why those four steps do not cost four round trips and is cheaper than it looks.
