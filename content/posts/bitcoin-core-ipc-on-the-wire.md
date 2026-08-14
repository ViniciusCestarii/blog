---
title: Bitcoin Core IPC on the Wire
date: 2026-08-14
toc: true
---

<nav class="series">
  <p>Part 3 of 3 on Bitcoin Core IPC. Background: <a href="/posts/introduction-to-bitcoin-core-ipc">Introduction to Bitcoin Core IPC</a>.</p>
  <ol>
    <li><a href="/posts/inside-bitcoin-core-ipc">Inside Bitcoin Core IPC</a></li>
    <li><a href="/posts/bitcoin-core-ipc-event-loop">Bitcoin Core IPC Event Loop</a></li>
    <li aria-current="page">Bitcoin Core IPC on the Wire</li>
  </ol>
</nav>

This is the third and last post on Bitcoin Core IPC. In the [first one](https://viniciuscestari.dev/posts/inside-bitcoin-core-ipc) I read the capnp schemas and predicted the exchange without looking at a byte. In the [second one](https://viniciuscestari.dev/posts/bitcoin-core-ipc-event-loop) I traced the wiring and found that the thread making an IPC call never touches the socket: it writes a wakeup byte on fd 12, and `b-capnp-loop` does the real work on fd 9.

Now we have every piece we need to read the trace. From here on it is all bytes: the bootstrap, `Init.construct`, `Init.makeWalletLoader`, and the reply that carries a wallet loader capability.

The trace here is the [same `strace` run as the last post](https://viniciuscestari.dev/posts/bitcoin-core-ipc-event-loop#tracing-ipc-communication), so each thread gets its own file named after its TID: `tracing.log.72388` is the `bitcoin-node` main thread, `tracing.log.72394` is `bitcoin-node` event loop, and `tracing.log.72393` is `bitcoin-wallet` main thread. And the fds are fixed: `bitcoin-node` talks to `bitcoin-wallet` on fd 9, `bitcoin-wallet` answers on fd 8, and fds 11 and 12 are the `bitcoin-node`'s internal wakeup channel, carrying single meaningless bytes to wake the loop up.

One fd index is overloaded and it is worth getting straight now, because it appears in the very first trace excerpt below. In `bitcoin-wallet`, fd 8 is the IPC socket. In `bitcoin-node`, fd 8 is the epoll instance: `bitcoin-node` closed the child's end of the socket pair right after forking, which freed the number 8, and the epoll instance created moments later got it. `strace -yy` tells them apart for us, printing `8<UNIX-STREAM:...>` for one and `8<anon_inode:[eventpoll]>` for the other.

## The bootstrap

Let's start with the very first message `bitcoin-node` sends to `bitcoin-wallet`, the bootstrap: asking for a capability that points to `Init`.

It starts in `bitcoin-node` main thread in `tracing.log.72388` and continues in `bitcoin-node` event loop thread in `tracing.log.72394`:

```bash
# tracing.log.72388, the main thread
11:53:22.283925 write(12<UNIX-STREAM:[508633->508632]>, "\0", 1) = 1 <0.000005>
11:53:22.283952 futex(0x56491b354d08, FUTEX_WAIT_BITSET_PRIVATE|..., 0, NULL, ...) = 0 <0.000276>

# tracing.log.72394, the event loop thread
11:53:22.284021 read(11<UNIX-STREAM:[508632->508633]>, "\0", 1) = 1 <0.000009>
11:53:22.284065 ioctl(9<UNIX-STREAM:[557321->557320]>, FIONBIO, [1]) = 0 <0.000006>
11:53:22.284096 ioctl(9<UNIX-STREAM:[557321->557320]>, FIOCLEX) = 0 <0.000006>
11:53:22.284124 epoll_ctl(8<anon_inode:[eventpoll]>, EPOLL_CTL_ADD, 9<UNIX-STREAM:[557321->557320]>, {events=EPOLLIN|EPOLLOUT|EPOLLRDHUP|EPOLLET, ...}) = 0 <0.000006>
11:53:22.284219 futex(0x56491b354d08, FUTEX_WAKE_PRIVATE, 2147483647) = 1 <0.000014>
```

So the main thread handed the event loop a piece of work and went to sleep waiting for it. That work is [`mp::ConnectStream`](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/libmultiprocess/include/mp/proxy-io.h#L831), the function that turns a bare file descriptor into a capnp connection:

```c++
template <typename InitInterface>
std::unique_ptr<ProxyClient<InitInterface>> ConnectStream(EventLoop& loop, int fd)
{
    typename InitInterface::Client init_client(nullptr);
    std::unique_ptr<Connection> connection;
    loop.sync([&] {
        auto stream =
            loop.m_io_context.lowLevelProvider->wrapSocketFd(fd, kj::LowLevelAsyncIoProvider::TAKE_OWNERSHIP);
        connection = std::make_unique<Connection>(loop, kj::mv(stream));
        init_client = connection->m_rpc_system->bootstrap(ServerVatId().vat_id).castAs<InitInterface>();
        // ...install a disconnect handler...
    });
    return std::make_unique<ProxyClient<InitInterface>>(
        kj::mv(init_client), connection.release(), /* destroy_connection= */ true);
}
```

The whole body is inside `loop.sync()`, and the trace above is that lambda running line by line. `sync()` itself is the first two lines: it writes the wakeup byte and then parks the main thread on a [`futex`](https://man7.org/linux/man-pages/man2/futex.2.html), `FUTEX_WAIT` on `0x56491b354d08`. The event loop thread picks the byte up and runs the lambda: `wrapSocketFd` is the `ioctl` and the `epoll_ctl` that adds fd 9, which is why fd 9 shows up in epoll only here and not when the event loop was built.

`m_rpc_system->bootstrap(...)` is what produces the message we are about to read. Then the lambda is done, and the event loop thread releases the main thread with `FUTEX_WAKE` on that same address.

Important to remember that Cap'n Proto [assumes its state is only ever touched from one thread](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/libmultiprocess/include/mp/proxy-io.h#L218), so `wrapSocketFd` and `bootstrap()` have to run on the event loop thread exactly like a method call does. Any thread that wants to touch the event loop goes through [`EventLoop::post()`](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/libmultiprocess/src/mp/proxy.cpp#L279), which stores the callback and writes the wakeup byte, or [`EventLoop::sync()`](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/libmultiprocess/include/mp/proxy-io.h#L272), which does the same.

For now, the wakeup byte is delivered, and the bootstrapping continues on the `bitcoin-node` event loop, in `tracing.log.72394`, with:

<div class="with-aside">

```bash
11:53:22.284435 writev(9<UNIX-STREAM:[557321->557320]>, [{iov_base="\0\0\0\0\5\0\0\0", iov_len=8}, {iov_base="\0\0\0\0...\0\0\0\0", iov_len=40}], 2) = 48 <0.000013>
```

<aside class="side">
  <p>The capnp payloads are long and mostly unprintable, so I am truncating them with <code>...</code> from here on. Byte counts and header bytes are untouched.</p>
</aside>

</div>

There is our first capnp message being sent. Note that it is a [`writev`](https://man7.org/linux/man-pages/man2/writev.2.html) and not a `write`: capnp keeps the message as a segment table plus the segments themselves, in separate buffers, and lets `writev` stitch them together in one syscall instead of copying them into one contiguous buffer first.

The 8 byte buffer is the capnp [segment table](https://capnproto.org/encoding.html#serialization-over-a-stream): first the segment count minus one, then the size of each segment in 8 byte words. Both are 32 bit fields, so those 8 bytes are two numbers, not one:

```
strace shows:  \0 \0 \0 \0   \5 \0 \0 \0
raw bytes:     00 00 00 00    05 00 00 00
               \_________/    \_________/
               segment count  segment size
               minus one = 0  = 5 words
```

Both fields are [little-endian](https://capnproto.org/encoding.html#serialization-over-a-stream), least significant byte first, so `05 00 00 00` reads as 0x00000005 = 5, not 0x05000000. That gives 5 words, and 5 * 8 = 40 bytes, exactly the size of the payload buffer that follows it.

capnp encodes every number little-endian, not just these two. Reading numbers backwards may seem like a weird choice for a wire format, but capnp is not only a wire format: the same bytes are also the in-memory representation, so it uses the byte order the CPU already wants and [there is no parsing step in between](https://capnproto.org/#:~:text=there%20is%20no%20encoding/decoding).

<div class="with-aside">

Now that we understand the segment table, we can read the message itself. You don't have a capnp decoder in your head? Me neither, but we can undo strace's `\ooo` escaping to get the raw bytes back and hand them to `capnp convert`, using [capnp's own RPC schema](https://github.com/capnproto/capnproto/blob/v1.2.0/c%2B%2B/src/capnp/rpc.capnp).

<aside class="side">
  <p>Cap'n Proto is two things: a serialization format, and an RPC protocol built on top of it. <code>rpc.capnp</code> is itself a capnp schema.</p>
</aside>

</div>

The schema isn't optional. Cap'n Proto's wire format is the struct's memory layout, so the bytes carry no field names at all. Four bytes at a given offset could be an integer, a float, or 32 booleans and nothing on the wire says which.

```bash
vinicius@archlinux ~> python3 oct2bin.py raw/1-bootstrap-call.txt 1-bootstrap-call.bin
capnp convert binary:text /usr/include/capnp/rpc.capnp Message < 1-bootstrap-call.bin
( bootstrap = (questionId = 0) )
```

The `oct2bin.py` script and every capnp message decoded in this post are [here](https://github.com/ViniciusCestarii/blog/tree/main/static/capnp/bitcoin-core-ipc-on-the-wire), if you want to follow along on the real bytes.

As we can see, the bootstrap is just a [simple struct](https://github.com/capnproto/capnproto/blob/v1.2.0/c++/src/capnp/rpc.capnp#L278):

```capnp
struct Bootstrap {
  questionId @0 :QuestionId;
  deprecatedObjectId @1 :AnyPointer;
}
```

The `questionId` is just an identifier of this request, which will eventually receive a `Return` message containing the requested capability.

So let's see `bitcoin-wallet` read it, in `tracing.log.72393`:

```bash
11:53:29.438836 read(8<UNIX-STREAM:[557320->557321]>, "\0\0\0\0\5\0\0\0...\0\0\0\0\0", 65536) = 216 <0.000069>
```

<div class="with-aside">

Worth noticing which file this showed up in. `tracing.log.72393` is the `bitcoin-wallet` main thread, because unlike `bitcoin-node`, `bitcoin-wallet` has no `b-capnp-loop`. It does not need one: [serving](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/capnp/protocol.cpp#L93) this connection is all it has to do, so it runs the very same `mp::EventLoop` on its main thread.

<aside class="side">
  <p>Which is why <code>ps -T</code> on <code>bitcoin-wallet</code> never shows a <code>b-capnp-loop</code>, even though it is running the same event loop code.</p>
</aside>

</div>

### Why `bitcoin-wallet` took seven seconds to read it

Notice the clock. We wrote at `11:53:22.284435` and `bitcoin-wallet` reads at `11:53:29.438836`, more than seven seconds later, while every other hop in this walkthrough is measured in microseconds. That gap is not `bitcoin-wallet` booting up, and it is not IPC being slow. It happens before `bitcoin-wallet` exists at all: the child spends it inside [`mp::SpawnProcess`](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/libmultiprocess/src/mp/util.cpp#L119), between `fork()` and `execve`, closing file descriptors.

```c++
    if (!pid) {
        // Child process must close all potentially open descriptors, except
        // socket 0. Do not throw, allocate, or do non-fork-safe work here.
        const int maxFd = MaxFd();
        for (int fd = 3; fd < maxFd; ++fd) {
            if (fd != fds[0]) {
                close(fd);
            }
        }
```

A forked child inherits the whole fd table, so before turning into another program it closes everything it did not mean to keep. [`MaxFd()`](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/libmultiprocess/src/mp/util.cpp#L52) is `RLIMIT_NOFILE` minus one, and the trace shows that mine is generous:

```bash
11:53:22.283224 prlimit64(0, RLIMIT_NOFILE, NULL, {rlim_cur=512*1024, rlim_max=512*1024}) = 0 <0.000018>
```

So the `for` loop runs from fd 3 to fd 524286: 524284 descriptors, and it calls `close` on every one of them but a single exception we will come back to. Five hit anything real:

```bash
11:53:22.283287 close(3<pipe:[552237]>) = 0 <0.000079>
11:53:22.283468 close(4<pipe:[552237]>) = 0 <0.000060>
11:53:22.283610 close(5</home/vinicius/.bitcoin/regtest/.lock>) = 0 <0.000060>
11:53:22.283767 close(6</home/vinicius/.bitcoin/regtest/blocks/.lock>) = 0 <0.000046>
11:53:22.283838 close(7</home/vinicius/.bitcoin/regtest/debug.log>) = 0 <0.000005>
```

Every other `close` comes back `EBADF`. Those 524k lines are the whole 7 seconds, and `execve` is the very next thing that happens:

```bash
11:53:22.283865 close(9)                = -1 EBADF (Bad file descriptor) <0.000012>
11:53:22.283916 close(10)               = -1 EBADF (Bad file descriptor) <0.000022>
11:53:22.283969 close(11)               = -1 EBADF (Bad file descriptor) <0.000005>
11:53:22.284001 close(12)               = -1 EBADF (Bad file descriptor) <0.000006>
...
11:53:29.431305 close(524284)           = -1 EBADF (Bad file descriptor) <0.000002>
11:53:29.431316 close(524285)           = -1 EBADF (Bad file descriptor) <0.000002>
11:53:29.431326 close(524286)           = -1 EBADF (Bad file descriptor) <0.000002>
11:53:29.431345 execve("/home/vinicius/Code/my/bitcoin/build/bin/bitcoin-wallet", ["/home/vinicius/Code/my/bitcoin/build/bin/bitcoin-wallet", "-ipcfd", "8"], 
```

Look at which number is missing from that sequence: it goes 7, then 9. `close(8)` never happens, because of the `if (fd != fds[0])` guard. The IPC socket is being deliberately spared, and it is the same fd 8 the child is about to be told to use with `-ipcfd 8`.

<div class="with-aside">

This only costs seven seconds because we are tracing it. `close` is one of the cheapest syscalls there is, but with `strace` tracing, a single `close` becomes handing control to `strace` and back each time, and then `strace` formats a line and writes it out. At 524k syscalls this becomes really apparent.

<aside class="side">
  <p>This is also why <code>tracing.log.72393</code> is 44+ MB, the biggest file in the folder by far.</p>
</aside>

</div>

Which is a good reminder for reading everything else here: `strace` is showing us real syscalls in a real order, but the clock between them is the clock of a program being watched, not of one running free.

## `Init.construct` and the `Finish` messages

Can you spot something weird here that we passed through? `bitcoin-node` wrote 48 bytes to the socket but `bitcoin-wallet` read 216 bytes.

It is because `bitcoin-node` wrote one more message to the socket before `bitcoin-wallet` read. Same pattern as before, a wakeup byte on fd 12 and then the event loop thread writing on fd 9:

```bash
# tracing.log.72388, the main thread
11:53:22.284626 write(12<UNIX-STREAM:[508633->508632]>, "\0", 1) = 1 <0.000006>

# tracing.log.72394, the event loop thread
11:53:22.284768 read(11<UNIX-STREAM:[508632->508633]>, "\0", 1) = 1 <0.000006>
11:53:22.285027 writev(9<UNIX-STREAM:[557321->557320]>, [{iov_base="\0\0\0\0\24\0\0\0", iov_len=8}, {iov_base="\0\0\0\0\...\0\0\0\0", iov_len=160}], 2) = 168 <0.000010>
```

Let's decode it too:

```bash
vinicius@archlinux ~> python3 oct2bin.py raw/2-construct-call.txt 2-construct-call.bin
capnp convert binary:text /usr/include/capnp/rpc.capnp Message < 2-construct-call.bin
( call = (
    questionId = 1,
    target = (
      promisedAnswer = (questionId = 0, transform = []) ),
    interfaceId = 9815814193794562661,
    methodId = 0,
    params = (
      content = <opaque pointer>,
      capTable = [
        (senderHosted = 0, attachedFd = 255) ] ),
    sendResultsTo = (caller = void),
    allowThirdPartyTailCall = false,
    noPromisePipelining = false,
    onlyPromisePipeline = false ) )
```

We can see it is a [`Call` struct](https://github.com/capnproto/capnproto/blob/v1.2.0/c++/src/capnp/rpc.capnp#L389) and it is calling `Init.construct`:

```capnp
interface Init $Proxy.wrap("interfaces::Init") {
    construct @0 (threadMap: Proxy.ThreadMap) -> (threadMap :Proxy.ThreadMap);
    ...
}
```

But how do we know it is `Init.construct`? It is because it includes `interfaceId = 9815814193794562661`, which converted to hex is `8838C700BA5CBA65`, and we can find this hex in the generated capnp code at `build/src/ipc/capnp/init.capnp.h` corresponding to the `Init` interface, and `methodId = 0` because it is `@0`.

Look again at the last line of `mp::ConnectStream`, the one that ran right after `bootstrap()`:

```c++
return std::make_unique<ProxyClient<InitInterface>>(
    kj::mv(init_client), connection.release(), /* destroy_connection= */ true);
```

That constructs the `Init` proxy client, and the [`ProxyClientBase` constructor](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/libmultiprocess/include/mp/proxy-io.h#L590) ends with:

```c++
Sub::construct(*this);
```

The base class provides an [empty default](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/libmultiprocess/include/mp/proxy.h#L129) for it, `static void construct(Super&) {}`, so for most proxy clients that line compiles to nothing. But `mpgen` [treats a method literally named `construct` as special](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/libmultiprocess/src/mp/gen.cpp#L542), and since `Init` declares one, the generated `ProxyClient<Init>` in `build/src/ipc/capnp/init.capnp.proxy-client.c++` overrides it with a body that sends a real RPC call, the same generated shape every other proxy method has. We will read one of those line by line when we get to `Init.makeWalletLoader`.

So creating the `Init` client object is what puts `Init.construct` on the wire: `mp::ConnectStream` returns a proxy object, and constructing that object is itself an IPC call.

Its purpose is to trade thread maps in both directions and each side records the other's in `Connection::m_thread_map`. That is the handshake that makes `ThreadMap.makeThread` possible in the next section.

But then how is `bitcoin-node` calling a method on a capability it doesn't have? Remember: we haven't yet read the boostrap return which will contain the capability for `Init`. The answer is in [`target.promisedAnswer`](https://github.com/capnproto/capnproto/blob/v1.2.0/c++/src/capnp/rpc.capnp#L967), which is used for referencing a capability from a call that has not been completed yet. This is capnp [promise pipelining](https://capnproto.org/rpc.html#time-travel-promise-pipelining).

Without it `bitcoin-node` would have to send the bootstrap, wait for the reply with the `Init` capability to come back, and only then write `Init.construct`. With it, both go out immediately, which is exactly what we saw on the wire: two `writev` calls at `11:53:22.284435` and `11:53:22.285027` with no `read` in between.

That leaves `content = <opaque pointer>`, the call's parameters. capnp is not being cagey there: in `rpc.capnp` the field is declared `content :AnyPointer`, because the RPC layer does not know what any given method's parameters look like. But we do, so we can tell it. Copy `rpc.capnp`, retype that one field as the params struct of the method we just identified:

```capnp
struct ConstructParams {
  threadMap @0 :Capability;
}
```

```bash
vinicius@archlinux ~> python3 oct2bin.py raw/2-construct-call.txt 2-construct-call.bin
capnp convert binary:text rpc-2-construct-call.capnp Message < 2-construct-call.bin
( call = (
    questionId = 1,
    target = (
      promisedAnswer = (questionId = 0, transform = []) ),
    interfaceId = 9815814193794562661,
    methodId = 0,
    params = (
      content = (
        threadMap = <opaque pointer> ),
      capTable = [
        (senderHosted = 0, attachedFd = 255) ] ),
    sendResultsTo = (caller = void),
    allowThirdPartyTailCall = false,
    noPromisePipelining = false,
    onlyPromisePipeline = false ) )
```

The patched schema is [here](https://github.com/ViniciusCestarii/blog/tree/main/static/capnp/bitcoin-core-ipc-on-the-wire). We now have a name for what is inside.

But `threadMap` is still `<opaque pointer>`, and this time capnp really cannot do better. A capability is not data. What sits in that pointer slot is a [capability pointer](https://capnproto.org/encoding.html#capabilities-interfaces): 2 bits saying "other pointer", 30 zero bits saying "capability", and a 32 bit index into the message's capability table. The object itself never crosses the socket. Here is that word, at offset 96 of the 168 bytes message `Init.construct` put on the wire:

```
03 00 00 00   00 00 00 00
|<- A + B ->| |<-- C -->|

A (2 bits)  = 3   this is an "other" pointer
B (30 bits) = 0   specifically a capability pointer
C (32 bits) = 0   index 0 in the message's capTable
```

Which is what [`capTable`](https://github.com/capnproto/capnproto/blob/v1.2.0/c++/src/capnp/rpc.capnp#L983) is for: it carries one descriptor per capability pointer in `content`. Entry 0 says `senderHosted = 0`, meaning the sender is the one hosting this capability, and 0 is its index in `bitcoin-node`'s export table and that number `bitcoin-wallet` will use to name `bitcoin-node`'s `ThreadMap` from now on.

The remaining field, `attachedFd = 255`, is capnp saying no file descriptor is attached. Cap'n Proto can hand an fd across a Unix socket alongside a capability, libmultiprocess does not use that here.

Which closes the arithmetic from before: the 216 bytes `bitcoin-wallet` read are 48 from the bootstrap plus 168 from `Init.construct`. And in the very next syscall the `bitcoin-wallet` main thread, in `tracing.log.72393`, answers both:

```bash
11:53:29.439159 writev(8<UNIX-STREAM:[557320->557321]>, [{iov_base="\0\0\0\0\v\0\0\0", iov_len=8}, {iov_base="\0\0\0\0\...\0\0\0\0", iov_len=88}, {iov_base="\0\0\0\0\f\0\0\0", iov_len=8}, {iov_base="\0\0\0\0...0\0\0\0", iov_len=96}], 4) = 200 <0.000020>
```

We can see two messages being passed because there are 2 segment tables and 2 payloads in the same `writev`, these are the replies to the bootstrap and to `Init.construct`.

Let's decode the first message reply:

```bash
vinicius@archlinux ~> python3 oct2bin.py raw/3-bootstrap-return.txt 3-bootstrap-return.bin
capnp convert binary:text /usr/include/capnp/rpc.capnp Message < 3-bootstrap-return.bin
( return = (
    answerId = 0,
    releaseParamCaps = true,
    results = (
      content = <opaque pointer>,
      capTable = [
        (senderHosted = 0, attachedFd = 255) ] ),
    noFinishNeeded = false ) )
```

We can see it is a [`Return`](https://github.com/capnproto/capnproto/blob/v1.2.0/c++/src/capnp/rpc.capnp#L505) struct and `answerId = 0`, which means it is answering `questionId = 0`, the bootstrap message, and we know its content is a capability for `Init`.

Now let's see the second message reply:

```bash
vinicius@archlinux ~> python3 oct2bin.py raw/4-construct-return.txt 4-construct-return.bin
capnp convert binary:text rpc-4-construct-return.capnp Message < 4-construct-return.bin 
( return = (
    answerId = 1,
    releaseParamCaps = false,
    results = (
      content = (
        threadMap = <opaque pointer> ),
      capTable = [
        (senderHosted = 1, attachedFd = 255) ] ),
    noFinishNeeded = false ) )
```

Same thing, a `Return` struct answering `questionId = 1`, which is the `Init.construct` message, and we see it returning a capability for `Proxy.ThreadMap`.

Then the `bitcoin-node` event loop thread, in `tracing.log.72394`, reads these replies and writes two new messages:

```bash
11:53:29.439218 read(9<UNIX-STREAM:[557321->557320]>, "\0\0\0\0...\0\0\0\0", 65536) = 200 <0.000011>
11:53:29.439633 writev(9<UNIX-STREAM:[557321->557320]>, [{iov_base="\0\0\0\0\4\0\0\0", iov_len=8}, {iov_base="\0\0\0\0...\3\0\0\0", iov_len=32}, {iov_base="\0\0\0\0\4\0\0\0", iov_len=8}, {iov_base="\0\0\0\0...\3\0\0\0", iov_len=32}], 4) = 80 <0.000006>
```

Decoding the first message:

```bash
vinicius@archlinux ~> python3 oct2bin.py raw/5-bootstrap-finish.txt 5-bootstrap-finish.bin
capnp convert binary:text /usr/include/capnp/rpc.capnp Message < 5-bootstrap-finish.bin
( finish = (
    questionId = 0,
    releaseResultCaps = false,
    requireEarlyCancellationWorkaround = false ) )
```

We can see it is a [`Finish`](https://github.com/capnproto/capnproto/blob/v1.2.0/c++/src/capnp/rpc.capnp#L569) struct, which serves to mark that the specified `questionId` will no longer be used in any messages sent by the callee, in this case the bootstrap call.

And the second message:

```bash
vinicius@archlinux ~> python3 oct2bin.py raw/6-construct-finish.txt 6-construct-finish.bin
capnp convert binary:text /usr/include/capnp/rpc.capnp Message < 6-construct-finish.bin
( finish = (
    questionId = 1,
    releaseResultCaps = false,
    requireEarlyCancellationWorkaround = false ) )
```

Also a `Finish` struct but now for the `Init.construct` call.

`bitcoin-wallet` doesn't need to answer `Finish` messages.

Ok, so all of this was the capnp bootstrap to get an `Init` capability and the libmultiprocess construct to exchange `Proxy.ThreadMap` capabilities: messages that exist only because these are two processes, with no counterpart in the monolith.

`Init.makeWalletLoader` is the first message where that changes, and that is what makes it the interesting one. In `bitcoind` it is an ordinary C++ call that returns a pointer. In `bitcoin-node` the same call, from the same call site, turns into bytes on a socket.

## `Init.makeWalletLoader`

It starts on the `bitcoin-node` main thread, in `tracing.log.72388`, with the wakeup byte:

```bash
11:53:29.439619 write(12<UNIX-STREAM:[508633->508632]>, "\0", 1) = 1 <0.000016>
11:53:29.439659 futex(0x56491b354d08, FUTEX_WAIT_BITSET_PRIVATE|FUTEX_CLOCK_REALTIME, 0, NULL, FUTEX_BITSET_MATCH_ANY) = 0 <0.000637>
```

One byte out, and then the main thread parks. Same shape as before, down to the address: `0x56491b354d08` again. Keep an eye on that futex, because we will come back to it and it does not mean what it looks like it means here.

Both of those syscalls come from the same place. The caller wrote `init->makeWalletLoader(...)`, which on a multiprocess build, since `init` is of type `ProxyClient<Init>`, resolves to a `ProxyClient` method that libmultiprocess generated at build time:

<div class="with-aside">

```c++
ProxyClient<ipc::capnp::messages::Init>::M7::Result ProxyClient<ipc::capnp::messages::Init>::makeWalletLoader(M7::Param<0> chain) {
    typename M7::Result result;
    clientInvoke(*this, &ipc::capnp::messages::Init::Client::makeWalletLoaderRequest, MakeClientParam<Accessor<init_fields::Context, FIELD_IN | FIELD_BOXED>>(), MakeClientParam<Accessor<init_fields::GlobalArgs, FIELD_IN | FIELD_BOXED>>(), MakeClientParam<Accessor<init_fields::Chain, FIELD_IN | FIELD_BOXED>>(M7::Fwd<0>(chain)), MakeClientParam<Accessor<init_fields::Result, FIELD_OUT | FIELD_BOXED>>(result));
    return result;
}
```

<aside class="side">
  <p>This one lives in <code>build/src/ipc/capnp/init.capnp.proxy-client.c++</code>, written during the build by the libmultiprocess <code>mpgen</code> code generator.</p>
</aside>

</div>

It reads scarier than it is. Every method comes out the same shape: declare a `result`, hand [`clientInvoke`](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/libmultiprocess/include/mp/proxy-types.h#L692) the capnp request to build plus one `MakeClientParam` per field, return the `result`. The `Accessor` template arguments are just flags that help libmultiprocess: `FIELD_IN` fields get copied into the request, `FIELD_OUT` ones get filled in from the response, which is how `result` comes back through an argument instead of a return value. Other `FIELD_*` are documented [here](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/libmultiprocess/include/mp/proxy.h#L315). So the whole generated file is a thin adapter, and all the interesting work is in `clientInvoke`, which is the same function for every method on every interface:

```c++
template <typename ProxyClient, typename GetRequest, typename... FieldObjs>
void clientInvoke(ProxyClient& proxy_client, const GetRequest& get_request, FieldObjs&&... fields)
{
    // ...
    ThreadContext& thread_context{g_thread_context};
    bool done = false;

    proxy_client.m_context.loop->sync([&]() {
        auto request = (proxy_client.m_client.*get_request)(nullptr);
        // ...convert the FIELD_IN arguments into the request...
        proxy_client.m_context.loop->m_task_set->add(request.send().then(
            [&](::capnp::Response<typename Request::Results>&& response) {
                // ...read the response and convert the FIELD_OUT fields into result
                done = true;
                thread_context.waiter->m_cv.notify_all();
            },
            // ...error handlers
                done = true;
                thread_context.waiter->m_cv.notify_all();
            ));
    });

    Lock lock(thread_context.waiter->m_mutex);
    thread_context.waiter->wait(lock, [&done]() { return done; });
}
```

The `sync()` call is the `write` of the wakeup byte: the calling thread cannot touch capnp state itself, so it hands a lambda to the event loop and waits for the event loop thread to run it. That lambda builds the request, sends it, and registers a continuation to run when the reply arrives. Then `clientInvoke` blocks on the waiter until that continuation flips `done`, which is a second, longer futex wait.

Then, in `tracing.log.72394`, the `bitcoin-node` event loop thread picks that byte up and gets to work:

<div class="with-aside">

```bash
11:53:29.439707 read(11<UNIX-STREAM:[508632->508633]>, "\0", 1) = 1 <0.000007>
11:53:29.440222 write(1</dev/pts/7>, "...[ipc] {bitcoin-node-72388/bitcoin-node-72388} IPC client send Init.makeWalletLoader$Params\n", 112) = 112 <0.000007>
11:53:29.440373 writev(9<UNIX-STREAM:[557321->557320]>, [{iov_base="\0\0\0\0\23\0\0\0", iov_len=8}, {iov_base="\0\0\0\0...\0bitcoin-node-72388/bitcoin-node-72388\0...0\0\0\0", iov_len=152}, {iov_base="\0\0\0\0C\0\0\0", iov_len=8}, {iov_base="\0\0\0\0\...\0/home/vinicius/.bitcoin/bitcoin.conf\0...\0\0\0\0", iov_len=536}], 4) = 704 <0.000008>
```

We can see two messages being passed because again there are 2 segment tables and 2 payloads here.

<aside class="side">
  <p>This is the first message where we can literally read something: capnp stores <code>Text</code> fields as plain bytes, and <code>strace</code> escapes only the bytes that are not printable, so the text comes out as text while everything around it stays <code>\ooo</code>.</p>
</aside>

</div>

Let's parse the first one with a patched schema:

```bash
vinicius@archlinux ~> python3 oct2bin.py raw/7-makethread-call.txt 7-makethread-call.bin
capnp convert binary:text rpc-7-makethread-call.capnp Message < 7-makethread-call.bin
( call = (
    questionId = 0,
    target = (importedCap = 1),
    interfaceId = 17176226603359394973,
    methodId = 0,
    params = (
      content = (
        name = "bitcoin-node-72388/bitcoin-node-72388" ) ),
    sendResultsTo = (caller = void),
    allowThirdPartyTailCall = false,
    noPromisePipelining = false,
    onlyPromisePipeline = false ) )
```

This is a `ThreadMap.makeThread` call, and this time the `target` is what tells us so. `importedCap = 1` means "the capability at 1 in my import table", and we have seen that number already: the `construct` reply carried `senderHosted = 1` in its `capTable`, which was `bitcoin-wallet` handing over its `ThreadMap`. The `bitcoin-wallet` export 1 is the `bitcoin-node` import 1. So the message reads: call method `@0` on that `ThreadMap`, and `@0` is `makeThread`:

```capnp
interface ThreadMap $count(0) {
    makeThread @0 (name :Text) -> (result :Thread);
    ...
}
```

And now the second message, also with a patched schema:

```bash
vinicius@archlinux ~> python3 oct2bin.py raw/8-makewalletloader-call.txt 8-makewalletloader-call.bin
capnp convert binary:text -I. rpc-8-makewalletloader-call.capnp Message < 8-makewalletloader-call.bin
( call = (
    questionId = 1,
    target = (importedCap = 0),
    interfaceId = 9815814193794562661,
    methodId = 7,
    params = (
      content = (
        context = <opaque pointer>,
        globalArgs = (
          settings = (
            forcedSettings = [
              (key = "server", value = "\"1\"") ],
            commandLineOptions = [
              (key = "debug", value = ["\"ipc\""]),
              (key = "regtest", value = ["\"\""]) ],
            rwSettings = [],
            roConfig = [
              ( key = "",
                value = [
                  (key = "prune", value = ["\"5000\""]) ] ) ] ),
          configPath = "/home/vinicius/.bitcoin/bitcoin.conf" ),
        chain = <opaque pointer> ),
      capTable = [
        ( receiverAnswer = (
            questionId = 0,
            transform = [(getPointerField = 0)] ),
          attachedFd = 255 ),
        (senderHosted = 1, attachedFd = 255),
        (senderHosted = 2, attachedFd = 255) ] ),
    sendResultsTo = (caller = void),
    allowThirdPartyTailCall = false,
    noPromisePipelining = false,
    onlyPromisePipeline = false ) )
```

This is the `Init.makeWalletLoader` call, and the target reads the same way. `importedCap = 0` is the `bitcoin-node` import 0, and that one goes all the way back to the bootstrap reply, which carried `senderHosted = 0`: the `Init` capability the whole connection starts from. So the message reads: call method `@7` on that `Init`, and `@7` is `makeWalletLoader`.

We can see `globalArgs` is the flags we started `bitcoin-node` with. `-debug=ipc` and `-regtest` are sitting in `commandLineOptions`, `prune=5000` came out of my `bitcoin.conf`, and `server=1` was forced on internally. `bitcoin-node` is handing `bitcoin-wallet` its entire view of the configuration, because `bitcoin-wallet` parses no config of its own.

There is one more thing hiding in these messages. Look back at the capTable of `Init.makeWalletLoader`: `receiverAnswer = (questionId = 0, ...)`. The `Init.makeWalletLoader` call is passing, as its context thread, a capability that does not exist yet: the answer to question 0, the `makeThread` call sitting right next to it in the same buffer. This is again capnp promise pipelining. `bitcoin-node` does not call `makeThread`, wait a full round trip, and only then call `makeWalletLoader` passing the `makeThread` response as an argument. It sends both at once and lets `bitcoin-wallet` resolve the reference on arrival, which is why two calls fit in one `writev` and a single round trip.

### The reply comes back

Now the other side. The `bitcoin-wallet` main thread, in `tracing.log.72393`, reads the same 704 bytes other end of the socket:

```bash
11:53:29.440420 read(8<UNIX-STREAM:[557320->557321]>, "...bitcoin-node-72388/bitcoin-node-72388...", 65536) = 704 <0.000008>
11:53:29.440604 clone3({flags=CLONE_VM|CLONE_THREAD|..., stack=0x7fe6795ff000, ...} => {parent_tid=[72454]}, 88) = 72454 <0.000034>
11:53:29.441426 writev(8<UNIX-STREAM:[557320->557321]>, [{iov_base="\0\0\0\0\f\0\0\0", iov_len=8}, {iov_base="\0\0\0\0...", iov_len=96}], 2) = 104 <0.000006>
```

We can see it read both messages in one go, and then call `clone3`. That is [`ProxyServer<ThreadMap>::makeThread`](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/libmultiprocess/src/mp/proxy.cpp#L443) answering the first of the two calls, spawning a thread that parks on a condition variable and waits to be handed work.

That thread has to exist because of the `context :Proxy.Context` parameter [we read in the first post](https://viniciuscestari.dev/posts/inside-bitcoin-core-ipc): every call carries one, and its `thread` field names the thread on the receiving side that should run the call. `Init.makeWalletLoader`, sitting in the very same buffer, names this one. That is what `receiverAnswer = (questionId = 0)` was doing: pointing at a thread that did not exist when `bitcoin-node` wrote those bytes, and that `clone3` has just now made real.

Then `bitcoin-wallet` main thread `writev(8)` replies on fd 8, and the `bitcoin-node` event loop thread reads it 83 microseconds after that:


```bash
11:53:29.441509 read(9<UNIX-STREAM:[557321->557320]>, "\0\0\0\0\f\0\0\0\0\0\0\0\1\0\1\0\3\0"..., 65536) = 104 <0.000008>
```

Let's decode it:

```bash
vinicius@archlinux ~> python3 oct2bin.py raw/9-makethread-return.txt 9-makethread-return.bin
capnp convert binary:text -I. rpc-9-makethread-return.capnp Message < 9-makethread-return.bin
( return = (
    answerId = 0,
    releaseParamCaps = false,
    results = (
      content = (
        result = <opaque pointer> ),
      capTable = [
        (senderHosted = 2, attachedFd = 255) ] ),
    noFinishNeeded = false ) )
```

`answerId = 0`. That is the answer to question 0, the `makeThread` call. `bitcoin-wallet` handed the capability back straight away, but `makeWalletLoader` was question 1, and it is still running.

Now the bitcoin-wallet main thread parks on epoll_wait while thread 72454 runs the `Init.makeWalletLoader` call. It sits there for 107 ms, then wakes and replies:

```bash
11:53:29.441871 epoll_wait(3<anon_inode:[eventpoll]>, [{events=EPOLLIN|EPOLLOUT, ...}], 16, -1) = 1 <0.107820>
11:53:29.550422 writev(8<UNIX-STREAM:[557320->557321]>, [{iov_base="\0\0\0\0\f\0\0\0", iov_len=8}, {iov_base="\0\0\0\0...\0\0\0\0", iov_len=96}], 2) = 104 <0.000009>
```

Then `bitcoin-node` event loop thread reads it:

```bash
11:53:29.550479 read(9<UNIX-STREAM:[557321->557320]>, "\0\0\0\0\f\0\0\0\0\0\0\0\1\0\1\0\3\0"..., 65536) = 104 <0.000008>
11:53:29.550552 write(1</dev/pts/7>, "...[ipc] {bitcoin-node-72388/bitcoin-node-72388} IPC client recv Init.makeWalletLoader$Results\n", 113) = 113 <0.000005> 
11:53:29.550773 futex(0x56491b36b92c, FUTEX_WAKE_PRIVATE, 2147483647) = 1 <0.000022>
```

And that one decodes to the answer we were actually waiting for:

```bash
vinicius@archlinux ~> python3 oct2bin.py raw/10-makewalletloader-return.txt 10-makewalletloader-return.bin
capnp convert binary:text -I. rpc-10-makewalletloader-return.capnp Message < 10-makewalletloader-return.bin
( return = (
    answerId = 1,
    releaseParamCaps = false,
    results = (
      content = (
        result = <opaque pointer> ),
      capTable = [
        (senderHosted = 3, attachedFd = 255) ] ),
    noFinishNeeded = false ) )
```

`answerId = 1`, so it is answering `Init.makeWalletLoader` with a `WalletLoader` capability, and only then does the event loop thread wake the main thread up.

### The two futexes

Which brings us back to two futexes the main thread parked on:

```bash
# tracing.log.72388, the main thread
11:53:29.439659 futex(0x56491b354d08, FUTEX_WAIT_BITSET_PRIVATE|..., 0, NULL, ...) = 0 <0.000637>
11:53:29.440365 futex(0x56491b36b92c, FUTEX_WAIT_BITSET_PRIVATE|..., 0, NULL, ...) = 0 <0.110424>

# tracing.log.72394, the event loop thread
11:53:29.440290 futex(0x56491b354d08, FUTEX_WAKE_PRIVATE, 2147483647) = 1 <0.000011>
11:53:29.550773 futex(0x56491b36b92c, FUTEX_WAKE_PRIVATE, 2147483647) = 1 <0.000022>
```

The first one, on `0x56491b354d08`, is the `sync()`, and it is released at `11:53:29.440290`, which is before the request was even written to fd 9 at `11:53:29.440373`. It blocks the calling thread only until the event loop thread has run its callback, which in this case just encodes the request and schedules it to be sent. It does not wait for anyone to answer.

The second one, on `0x56491b36b92c`, is the actual wait for the result, and it lasts 110.4 milliseconds, ending right at the `FUTEX_WAKE` that follows the read of what it wanted: `makeWalletLoader Return`.

Remember the last two lines of [`clientInvoke`](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/libmultiprocess/include/mp/proxy-types.h#L768), the function every generated proxy method calls:

```c++
    Lock lock(thread_context.waiter->m_mutex);
    thread_context.waiter->wait(lock, [&done]() { return done; });
```

And `done` only flips inside the [continuation](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/libmultiprocess/include/mp/proxy-types.h#L737) that the event loop thread runs when the response lands:

```c++
  proxy_client.m_context.loop->m_task_set->add(request.send().then(
      [&](::capnp::Response<typename Request::Results>&& response) {
          // ...read the response and convert the FIELD_OUT fields into result
          done = true;
          thread_context.waiter->m_cv.notify_all();
      },
      // ...error handlers
```

[`Waiter::wait`](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/libmultiprocess/include/mp/proxy-io.h#L400) is a `std::condition_variable`, and on Linux that is a futex. Both futexes we have seen in this trace are one of these: `0x56491b36b92c` is this `Waiter`, and `0x56491b354d08` back at the bootstrap is the event loop's own.

### Where the 110 milliseconds actually went

Do not read those 110.4 milliseconds as the cost of IPC. Nearly all of them sit in a single gap in the trace of the `bitcoin-wallet` thread that ran the call, `tracing.log.72454`, and the gap contains no syscalls at all:

```bash
11:53:29.447668 getegid()               = 1000 <0.000004>
# ...100.187 ms, not a single syscall...
11:53:29.547859 futex(0x560b18fa41d8, FUTEX_WAKE_PRIVATE, 2147483647) = 0 <0.000161>
```

An empty stretch of trace is still evidence. It means that the culprit was running, on CPU, in code that had no reason to ask the kernel for anything.

`getegid()` is the fingerprint. It is the last syscall of [`RandAddStaticEnv`](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/randomenv.cpp#L471), and the syscalls before it are the rest of that same function scraping the machine for entropy: `/proc/diskstats`, `/proc/vmstat`, `/proc/meminfo`, `uname`, a netlink dump of every network interface. The statement right after it, in [`SeedStartup`](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/random.cpp#L552), is where the thread went quiet:

```c++
    // Strengthen for 100 ms
    SeedStrengthen(hasher, rng, 100ms);
```

[`Strengthen`](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/random.cpp#L235) is a SHA512 loop on a wall clock budget: hash a thousand times, check whether 100 ms have gone by, repeat.

So why is Bitcoin Core seeding its RNG in the middle of our call? Because `bitcoin-wallet` parses no configuration of its own, and does almost nothing until the node's arrives. The `globalArgs` we decoded above is what arrives, and unpacking it is not a plain field copy: on the server side each parameter goes through `CustomPassField`, which Bitcoin Core [specializes for `GlobalArgs`](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/capnp/common-types.h#L268) so that reading the field does more than read the field:

```c++
    ipc::capnp::ReadGlobalArgs(server_context, Accessor::get(server_context.call_context.getParams()));
    return fn.invoke(server_context, std::forward<Args>(args)...);
```

So before `makeWalletLoader` itself ever runs, [`ReadGlobalArgs`](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/capnp/common.cpp#L39) loads those settings into `gArgs`, selects the chain, and calls `init_process()`, which for `bitcoin-wallet` is [this](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/init/bitcoin-wallet-ipc.cpp#L52):

```c++
BitcoinWalletInit(const char* arg0) : m_ipc(interfaces::MakeIpc(EXE_NAME, ".wallet", arg0, *this))
{
    // Extra initialization code that runs when a bitcoin-wallet process is
    // spawned by a bitcoin-node process, after the ArgsManager
    // configuration is transferred from the parent process to the child
    // process. This is a subset of the initialization done in bitcoind
    // AppInit() function, doing only initialization needed by the wallet.
    m_ipc->context().init_process = [this] {
        m_kernel.emplace();
        m_ecc_context.emplace();
        init::SetLoggingOptions(gArgs, m_ipc->logSuffix());
        if (auto result = init::SetLoggingCategories(gArgs); !result) {
            throw std::runtime_error(util::ErrorString(result).original);
        }
        if (!init::StartLogging(gArgs)) {
            throw std::runtime_error("Logging start failure");
        }
    };
}
```

`m_kernel.emplace()` is the one that gets us there: [`kernel::Context`](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/kernel/context.cpp#L15) runs `RandomInit()` inside a `std::call_once`, and that first use of the RNG is what triggers `SeedStartup`.

That is the whole 110.4: 0.6 ms to start the thread, 6.7 ms of entropy gathering, 100.2 ms of strengthening, then 2.9 ms of logging setup and the handoff back to the event loop that writes the `Return`.

And unlike the seven seconds of `close`, `Strengthen` costs 100 ms traced or untraced, and `bitcoind` pays it at startup just the same. What the split changes is where it lands. The wallet process cannot initialize until the node's configuration reaches it, so a startup cost turns into the tail of the first IPC call.

## The threads that appear along the way

The call `ThreadMap.makeThread` deserves a close look. Let's see it run again. With `bitcoin-node` and `bitcoin-wallet` running, let's give them something to chew on:

```bash
build/bin/bitcoin-cli -regtest createwallet hello
```

Now if we check the number of threads of `bitcoin-wallet`:

```bash
vinicius@archlinux ~> ps -T -p $(pidof bitcoin-wallet) -o tid,comm
    TID COMMAND
  72393 bitcoin-wallet
  72454 bitcoin-wallet
  72503 b-schedqueue
  72603 bitcoin-wallet # this is new!
```

One new thread. That was created by `ThreadMap.makeThread` and it is libmultiprocess creating a per-client-thread server thread: every thread that makes an IPC call gets a dedicated worker on the other side, created on first use and kept for the lifetime of the calling thread.

The mechanism is in [`BuildField`](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/libmultiprocess/include/mp/type-context.h#L15) specialization for the `mp.Context` argument that prepends the majority of Bitcoin Core capnp schemas. It looks up `thread_context.request_threads[connection]`, and on a miss it sends `ThreadMap.makeThread`, passing the caller's own name, which is exactly the `bitcoin-node-72388/bitcoin-node-72388` string we could read in the payload:

```c++
auto request = connection.m_thread_map.makeThreadRequest();
request.setName(thread_context.thread_name);
```

On the receiving side, [`ProxyServer<ThreadMap>::makeThread`](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/libmultiprocess/src/mp/proxy.cpp#L443), the server implementation, spawns a `std::thread` that parks on a condition variable, and every later call from that same calling thread is dispatched onto it that we have seen.

So for the new `bitcoin-wallet` thread: `bitcoin-cli` sends JSON-RPC over HTTP to `bitcoin-node`, which handles the request on one of its `b-http.NN` pool threads. `createwallet` is a wallet RPC, so it is forwarded to `bitcoin-wallet`, and that particular HTTP worker thread had never talked to `bitcoin-wallet` before. Hence one new thread in `bitcoin-wallet`.

These threads do not go away when the RPC returns. The worker exits only when [`~ProxyServer<Thread>`](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/libmultiprocess/src/mp/proxy.cpp#L383) clears its waiter, which happens when the calling thread drops its `ProxyClient<Thread>`. HTTP workers live until shutdown, so the count only grows. If you keep calling `createwallet` from different connections you will see `bitcoin-wallet` climb toward 16 threads and then stop, once every HTTP worker has its counterpart.

```bash
build/bin/bitcoin-cli -regtest createwallet hello2
build/bin/bitcoin-cli -regtest createwallet hello3
build/bin/bitcoin-cli -regtest createwallet hello4
build/bin/bitcoin-cli -regtest createwallet hello5
build/bin/bitcoin-cli -regtest createwallet hello6
build/bin/bitcoin-cli -regtest createwallet hello7
build/bin/bitcoin-cli -regtest createwallet hello8
build/bin/bitcoin-cli -regtest createwallet hello9
```

```bash
vinicius@archlinux ~> ps -T -p $(pidof bitcoin-wallet) -o tid,comm
    TID COMMAND
  72393 bitcoin-wallet
  72454 bitcoin-wallet
  72503 b-schedqueue
  72603 bitcoin-wallet
  72633 bitcoin-wallet  # this is new!
  72635 bitcoin-wallet  # this is new!
  72637 bitcoin-wallet  # this is new!
  72639 bitcoin-wallet  # this is new!
  72641 bitcoin-wallet  # this is new!
  72643 bitcoin-wallet  # this is new!
  72645 bitcoin-wallet  # this is new!
  72647 bitcoin-wallet  # this is new!
```

## One loose end: every read comes in pairs

I owe you the reason the excerpts above are trimmed. Every `read` by the event loop that returns bytes is followed by another one on the same fd that fails:

```bash
11:53:29.438836 read(8<UNIX-STREAM:[557320->557321]>, "\0\0\0\0\...\0\0\0\0", 65536) = 216 <0.000069>
11:53:29.439041 read(8<UNIX-STREAM:[557320->557321]>, 0x560b242ba850, 65536) = -1 EAGAIN (Resource temporarily unavailable) <0.000007>
```

It is on purpose. KJ registers every socket it watches with [`EPOLLET`](https://github.com/capnproto/capnproto/blob/v1.2.0/c%2B%2B/src/kj/async-unix.c%2B%2B#L458), epoll's edge triggered mode, which reports only the *transition* from "nothing to read" to "something to read". If the event loop stopped reading while bytes were still in the receive queue, there would be no new transition to report, and `epoll_wait` would never mention that fd again even though a whole message was sitting there.

So KJ never parks on a socket it has not just drained: it reads, and [only once a read fails with `EAGAIN`](https://github.com/capnproto/capnproto/blob/v1.2.0/c%2B%2B/src/kj/async-io-unix.c%2B%2B#L718) does it wait for readability again. That `EAGAIN` is the only proof the kernel can give that the queue is empty. Which is why I showed you only the first read of each pair: the second one never carries anything.

## Wrapping up

We have read the first 10 capnp messages between `bitcoin-node` and `bitcoin-wallet`: a bootstrap, an `Init.construct`, a `ThreadMap.makeThread`, an `Init.makeWalletLoader`, their 4 returns and 2 finishes. What came back at the end was not a wallet loader. It was `senderHosted = 3`, a capability: an index into an export table in another process, which `bitcoin-node` will call methods on as if it were an ordinary C++ pointer.

This entire exchange we traced was one call. `WalletInit::Construct()` runs `init->makeWalletLoader(*node.chain)` once at startup, and that single line turned into a socket pair, a fork, a bootstrap, a thread created in another process, 704 bytes of `writev`, futex waits, event loops. 

Every call follows the same path, and the thread that made it is asleep for all but the first step and the last:

1. The `bitcoin-node` calling thread hands the work to the event loop thread `b-capnp-loop`, writes one byte to fd 12, and parks on a condition variable.
2. The event loop wakes by that one byte, encodes the request, and writes it to fd 9.
3. On the other side, `bitcoin-wallet`'s main thread event loop reads it on fd 8.
4. It dispatches to the thread named by `context.thread`, created on that caller's first call and reused ever after.
5. That thread runs the actual method.
6. It cannot write the answer itself, so it hands it back to its own event loop and goes back to waiting for the next call.
7. The `bitcoin-wallet` event loop wakes by that one byte and writes the `Return` on fd 8.
8. `bitcoin-node` `b-capnp-loop` event loop thread reads it on fd 9, decodes it, and flips `done` to true.
9. The `bitcoin-node` calling thread wakes with a result in hand, having never touched a socket and continue its execution.

Two processes, four threads, and the only two that ever touch the socket are the event loops.

The four steps [predicted from the schemas](https://viniciuscestari.dev/posts/inside-bitcoin-core-ipc) did not cost four round trips, they cost two, because capnp lets you pipeline and `bitcoin-node` did it twice.

The bootstrap and `Init.construct` went out back to back with no wait in between, which is why `construct`'s target was `promisedAnswer = (questionId = 0)`: a method call on the `Init` capability, sent before `bitcoin-node` had been given it. Both answers came back together in a single 200 byte read. Then `makeThread` and `makeWalletLoader` did the same thing in one 704 byte `writev`, the second one naming the answer to the first before that answer existed. Four calls, two trips.

That is the achievement of good design and of the effort put into it over the years, most of it by [Ryan Ofsky](https://github.com/ryanofsky), who opened [PR #10102](https://github.com/bitcoin/bitcoin/pull/10102) and has been carrying the multiprocess work ever since. The parts that feel expensive, the serialization and the syscalls, are microseconds. The parts that are actually expensive are the same parts that were expensive in the monolith. What the split really costs is not speed, it is that passing a reference and getting back a pointer is now a distributed system, with an export table, a lifetime protocol, and a thread on the other side with your name on it.

And if you got here from the first post: congratulations! Three posts, all to watch one line of C++ do what in `bitcoind` costs one reference in and one pointer out and nothing else (look what I have become). Thanks for reading to the end.

All ten messages, and the `oct2bin.py` script that unescapes them, are [here](https://github.com/ViniciusCestarii/blog/tree/main/static/capnp/bitcoin-core-ipc-on-the-wire).
