---
title: Bitcoin Core IPC Event Loop
date: 2026-08-10
toc: true
---

<nav class="series">
  <p>Part 2 of 3 on Bitcoin Core IPC. Background: <a href="/posts/introduction-to-bitcoin-core-ipc">Introduction to Bitcoin Core IPC</a>.</p>
  <ol>
    <li><a href="/posts/inside-bitcoin-core-ipc">Inside Bitcoin Core IPC</a></li>
    <li aria-current="page">Bitcoin Core IPC Event Loop</li>
    <li><a href="/posts/bitcoin-core-ipc-on-the-wire">Bitcoin Core IPC on the Wire</a></li>
  </ol>
</nav>

In [the last post](https://viniciuscestari.dev/posts/inside-bitcoin-core-ipc) I set up multiprocess Bitcoin Core, watched `bitcoin-node` spawn `bitcoin-wallet`, and read the capnp schemas to work out what the two processes have to say to each other before a wallet loader exists.

This post is about everything that has to be true before the first byte can be read: which file descriptor each process ends up holding, which one you read to get what the other side wrote, and which thread does the reading and writing. That last one has a surprising answer, and it is where we end up.

To follow along you need the multiprocess build, which [Setting Up](https://viniciuscestari.dev/posts/inside-bitcoin-core-ipc#setting-up) in the last post covers.

## Tracing IPC Communication

Everything we are after happens in the first few hundred milliseconds of the process's life: the socket pair, the fork, the bootstrap. By the time a node is up and idle it is long over. So we cannot attach to a running `bitcoin-node`, we have to launch it under `strace` and catch it from its very first syscall.

<div class="with-aside">

Which means stopping the one you already have running, if any:

```bash
kill $(pidof bitcoin-node)
```

<aside class="side">
  <p>This stops <code>bitcoin-wallet</code> too: it notices the Unix Domain socket is dead and <a href="https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/bitcoin-wallet.cpp#L102">exits normally</a>.</p>
</aside>

</div>

<div class="with-aside">

Now that `bitcoin-node` and `bitcoin-wallet` are down let's run again and trace it with `strace`!

```bash
mkdir trace
strace -ff -o trace/tracing.log -tt -T -yy -s 999999 build/bin/bitcoin -m node -debug=ipc -regtest
```

<aside class="side">
  <p>Sorry, this post is Linux heavy. <code>strace</code> is Linux only, so you will need a Linux box to follow along.</p>
</aside>

</div>

The flags of `strace` set here are:

| Flag | Meaning |
|---|---|
| `-ff` | Follow forks, useful for tracing `bitcoin-wallet` too |
| `-o trace/tracing.log` | Send trace output to file |
| `-tt` | Print absolute timestamp |
| `-T` | Print time spent in each syscall |
| `-yy` | Print all available info for file descriptor arguments |
| `-s 999999` | Limit length of printed strings (raised high to avoid truncation) |

<div class="with-aside">

Tracing Bitcoin Core like this can make BIG files.

<aside class="side">
  <p>I am not publishing my trace, and you should not publish yours: it records sensitive information and is a great machine fingerprint.</p>
</aside>

</div>

Now if we check the files in the folder `trace`:

```bash
vinicius@archlinux ~> ls trace
tracing.log.72388  tracing.log.72461  tracing.log.72472  tracing.log.72483  tracing.log.72494
tracing.log.72392  tracing.log.72462  tracing.log.72473  tracing.log.72484  tracing.log.72495
tracing.log.72393  tracing.log.72463  tracing.log.72474  tracing.log.72485  tracing.log.72496
tracing.log.72394  tracing.log.72464  tracing.log.72475  tracing.log.72486  tracing.log.72497
tracing.log.72454  tracing.log.72465  tracing.log.72476  tracing.log.72487  tracing.log.72498
tracing.log.72455  tracing.log.72466  tracing.log.72477  tracing.log.72488  tracing.log.72499
tracing.log.72456  tracing.log.72467  tracing.log.72478  tracing.log.72489  tracing.log.72500
tracing.log.72457  tracing.log.72468  tracing.log.72479  tracing.log.72490  tracing.log.72501
tracing.log.72458  tracing.log.72469  tracing.log.72480  tracing.log.72491  tracing.log.72502
tracing.log.72459  tracing.log.72470  tracing.log.72481  tracing.log.72492  tracing.log.72503
tracing.log.72460  tracing.log.72471  tracing.log.72482  tracing.log.72493  tracing.log.76993
```

That's a lot of files. Both `bitcoin-node` and `bitcoin-wallet` traces land in the same folder, told apart only by TID (thread id). So we need to figure out which thread belongs to whom.

That's easy: we can run the following to get the TID and command name of every current `bitcoin-node` thread:

```bash
vinicius@archlinux ~> ps -T -p $(pidof bitcoin-node) -o tid,comm
    TID COMMAND
  72388 bitcoin-node
  72392 b-scheduler
  72394 b-capnp-loop
  72455 b-http.00
  72456 b-http.01
  72457 b-http.02
  72458 b-http.03
  72459 b-http.04
  72460 b-http.05
  72461 b-http.06
  72462 b-http.07
  72463 b-http.08
  72464 b-http.09
  72465 b-http.10
  72466 b-http.11
  72467 b-http.12
  72468 b-http.13
  72469 b-http.14
  72470 b-http.15
  72471 b-http
  72472 b-scriptch.00
  72473 b-scriptch.01
  72474 b-scriptch.02
  72475 b-scriptch.03
  72476 b-scriptch.04
  72477 b-scriptch.05
  72478 b-scriptch.06
  72479 b-scriptch.07
  72480 b-scriptch.08
  72481 b-scriptch.09
  72482 b-scriptch.10
  72483 b-scriptch.11
  72484 b-scriptch.12
  72485 b-scriptch.13
  72486 b-scriptch.14
  72487 b-prevout.00
  72488 b-prevout.01
  72489 b-prevout.02
  72490 b-prevout.03
  72491 b-prevout.04
  72492 b-prevout.05
  72493 b-prevout.06
  72494 b-prevout.07
  72496 b-mapport
  72497 b-torcontrol
  72498 b-net
  72500 b-addcon
  72501 b-opencon
  72502 b-msghand
```

There are two interesting things in it. The first is that every thread besides `bitcoin-node` starts with `b-`, a Bitcoin Core convention where every thread it creates is prefixed with `b-`. So why doesn't `bitcoin-node` start with `b-`? Because it is the main thread: it was the first thread created (lowest TID), it came with the process itself, and the main thread always takes the program name.

The second is `b-capnp-loop`, sitting there at TID 72394 among the schedulers and HTTP workers. By the end of this post we will understand its function.

Let's find out the same for `bitcoin-wallet` too:

<div class="with-aside">

```bash
vinicius@archlinux ~> ps -T -p $(pidof bitcoin-wallet) -o tid,comm
    TID COMMAND
  72393 bitcoin-wallet
  72454 bitcoin-wallet
  72503 b-schedqueue
```

Ok, so we found all the threads!

<aside class="side">
  <p>A new thread always inherits its creator's name unless renamed, hence two <code>bitcoin-wallet</code> here.</p>
</aside>

</div>

The IPC communication runs over a Unix Domain socket, and a socket is reached through a file descriptor, so let's find it with `lsof`:

```bash
vinicius@archlinux ~> lsof -p $(pidof bitcoin-node) -a -U
COMMAND     PID     USER FD   TYPE             DEVICE SIZE/OFF   NODE NAME
bitcoin-n 72388 vinicius  9u  unix 0x00000000874de7ee      0t0 557321 type=STREAM (CONNECTED)
bitcoin-n 72388 vinicius 11u  unix 0x000000002ef26c3a      0t0 508632 type=STREAM (CONNECTED)
bitcoin-n 72388 vinicius 12u  unix 0x0000000096e99a46      0t0 508633 type=STREAM (CONNECTED)
```

But why did 3 show up? Two of them are a socket pair that libmultiprocess keeps to wake up its event loop, and we will get to it later. The other is our end of the Unix Domain socket used to talk to `bitcoin-wallet`, and it shows up alone because its peer lives in the other process.

But which one is which? Look at the `NODE` column, which holds the inode of each socket (an fd is only a per process index, the kernel resolves it to an open file and from there to the inode). fd 11 and fd 12 have adjacent inodes, 508632 and 508633, so they were created one right after the other, which is exactly what the [`socketpair`](https://man7.org/linux/man-pages/man2/socketpair.2.html) syscall does, as we will see in the trace. That leaves fd 9 as the end we care about, the one used for the IPC communication with `bitcoin-wallet`.

And for `bitcoin-wallet`:

```bash
vinicius@archlinux ~> lsof -p $(pidof bitcoin-wallet) -a -U
COMMAND     PID     USER FD   TYPE             DEVICE SIZE/OFF   NODE NAME
bitcoin-w 72393 vinicius 5u  unix 0x0000000066751344      0t0 552248 type=STREAM (CONNECTED)
bitcoin-w 72393 vinicius 6u  unix 0x00000000e436403a      0t0 552249 type=STREAM (CONNECTED)
bitcoin-w 72393 vinicius 8u  unix 0x000000006840f9c7      0t0 557320 type=STREAM (CONNECTED)
```

Here 3 showed up for the same reason: 2 are for the libmultiprocess event loop, and the other is the one we care about. This time there is no mystery, because `bitcoin-wallet` announces the answer in its own command line. `bitcoin-node` spawns it as `bitcoin-wallet -ipcfd 8`, and that flag is exactly how the child gets told which descriptor to use to talk back to its parent. So fd 8 is the one.

And it checks out from the other side too: fd 8 here has inode 557320 and fd 9 on `bitcoin-node` has inode 557321, consecutive again, because both came out of the same `socketpair` call in the parent before the child was spawned.

Which raises a fair question: how can `bitcoin-wallet` use an fd created before it even existed? Because `fork()` copies the parent's file descriptor table into the child, so every fd the parent had open is still open in the child, at the same number. The parent creates the pair, forks, and then only has to tell the child which number to look at, which is what `-ipcfd 8` is.

Now the trace itself. I will start with the `tracing.log` with the lowest TID, `tracing.log.72388`, because that is the main thread of `bitcoin-node`.

Right at the top we can see the [`execve`](https://man7.org/linux/man-pages/man2/execve.2.html) syscall that `bitcoin` runs so it replaces its image with the `bitcoin-node` one, with the args that we passed.

```bash
11:53:22.146392 execve("/home/vinicius/Code/my/bitcoin/build/bin/bitcoin-node", ["/home/vinicius/Code/my/bitcoin/build/bin/bitcoin-node", "-debug=ipc", "-regtest"], 0x7ffecb298928 /* 64 vars */) = 0 <0.000366>
```

On `bitcoin-node` startup it will try to construct a wallet loader via `WalletInit::Construct()`:

```cpp
void WalletInit::Construct(NodeContext& node) const
{
    ArgsManager& args = *Assert(node.args);
    if (args.GetBoolArg("-disablewallet", DEFAULT_DISABLE_WALLET)) {
        LogInfo("Wallet disabled!");
        return;
    }
    auto wallet_loader = node.init->makeWalletLoader(*node.chain);
    if (!wallet_loader) {
        // If the current process doesn't have wallet support linked in, spawn
        // a new wallet process.
        auto init = node.init->ipc()->spawnProcess("bitcoin-wallet");
        wallet_loader = init->makeWalletLoader(*node.chain);
        node.init->ipc()->addCleanup(*wallet_loader, [init = init.release()] { delete init; });
    }
    node.wallet_loader = wallet_loader.get();
    node.chain_clients.emplace_back(std::move(wallet_loader));
}
```

This is the main difference between `bitcoin-node` and `bitcoind` (the monolith): `bitcoin-node` does not offer an in-process wallet loader, so it spawns `bitcoin-wallet`, talks to it via IPC and asks it for a wallet loader.

The comment says "linked in", but for `bitcoin-node` that is not quite what happens: [both executables link the same wallet library](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/CMakeLists.txt#L338), and they differ only in which `Init` class they are built with. [`BitcoindInit`](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/init/bitcoind.cpp#L36) overrides `makeWalletLoader` and hands back an in-process loader, while [`BitcoinNodeInit`](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/init/bitcoin-node.cpp#L34) does not override it at all, so it inherits [the base version that returns `nullptr`](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/interfaces/init.h#L38). That `nullptr` is what sends us into `spawnProcess("bitcoin-wallet")` branch.

`spawnProcess()` calls [`mp::SpawnProcess`](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/libmultiprocess/src/mp/util.cpp#L119), which explains the next syscalls:

<div class="with-aside">

We can see `bitcoin-node` generating the Unix Domain socket pair via `socketpair` syscall:

```bash
11:53:22.282667 socketpair(AF_UNIX, SOCK_STREAM, 0, [8<UNIX-STREAM:[557320->557321]>, 9<UNIX-STREAM:[557321->557320]>]) = 0 <0.000010>
```

<aside class="side">
  <p>The 4th argument is where the syscall writes its result: an <code>int</code> array of 2. <code>strace</code> prints it already filled in, so we get both fds, 8 and 9.</p>
</aside>

</div>

And then the [`fork()`](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/libmultiprocess/src/mp/util.cpp#L135) to create a child process that will become `bitcoin-wallet`:

```bash
11:53:22.282823 clone(child_stack=NULL, flags=CLONE_CHILD_CLEARTID|CLONE_CHILD_SETTID|SIGCHLD, child_tidptr=0x7f1574d5bb90) = 72393 <0.000144>
```

Something interesting here: [the code calls `fork()`](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/libmultiprocess/src/mp/util.cpp#L135), but the trace shows `clone` syscall. That is because on Linux, glibc implements `fork()` with the `clone` syscall instead of the `fork` syscall.

`clone` returns the PID of the new child process and here it is: PID 72393. On Linux the main thread of a process always has a TID equal to the PID, so when I start tracing `bitcoin-wallet` I will start with the file `tracing.log.72393`!

Then `bitcoin-node` [closes the child's end of the Unix Domain socket](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/libmultiprocess/src/mp/util.cpp#L142):

```bash
11:53:22.283059 close(8<UNIX-STREAM:[557320->557321]>) = 0 <0.000007>
```

And the [child closes the parent end](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/libmultiprocess/src/mp/util.cpp#L142), in this case `close(9)`.

I have made this interactive visualization to make this wiring flow clearer:

<style>
figure.sp {
  position: relative;
  margin: 2rem 0;
  border: 1px solid var(--rule);
  padding: 1rem;
}
figure.sp input {
  position: absolute;
  opacity: 0;
  pointer-events: none;
}
figure.sp .sp-steps {
  display: flex;
  flex-wrap: wrap;
  gap: 0.4rem;
  margin-bottom: 1.25rem;
}
figure.sp .sp-steps label {
  font-family: monospace;
  font-size: 0.8rem;
  line-height: 1;
  padding: 0.35rem 0.6rem;
  border: 1px solid var(--rule);
  color: var(--muted);
  cursor: pointer;
  transition: color 0.2s ease, background 0.2s ease, border-color 0.2s ease;
}
figure.sp .sp-steps label:hover {
  color: var(--accent);
  border-color: var(--accent);
}
figure.sp #sp1:focus-visible ~ .sp-steps label[for="sp1"],
figure.sp #sp2:focus-visible ~ .sp-steps label[for="sp2"],
figure.sp #sp3:focus-visible ~ .sp-steps label[for="sp3"],
figure.sp #sp4:focus-visible ~ .sp-steps label[for="sp4"] {
  outline: 2px solid var(--accent);
  outline-offset: 2px;
}
figure.sp #sp1:checked ~ .sp-steps label[for="sp1"],
figure.sp #sp2:checked ~ .sp-steps label[for="sp2"],
figure.sp #sp3:checked ~ .sp-steps label[for="sp3"],
figure.sp #sp4:checked ~ .sp-steps label[for="sp4"] {
  color: var(--bg);
  background: var(--fg);
  border-color: var(--fg);
}
figure.sp .sp-grid {
  display: grid;
  grid-template-columns: 1fr auto 1fr;
  align-items: center;
  justify-items: center;
  gap: 0.5rem 0.75rem;
  font-family: monospace;
  font-size: 0.85rem;
}
figure.sp .sp-head {
  color: var(--muted);
  font-size: 0.75rem;
}
figure.sp .sp-head.child {
  visibility: hidden;
  opacity: 0;
  transition: opacity 0.35s ease, visibility 0s linear 0.35s;
}
figure.sp #sp2:checked ~ .sp-grid .sp-head.child,
figure.sp #sp3:checked ~ .sp-grid .sp-head.child,
figure.sp #sp4:checked ~ .sp-grid .sp-head.child {
  visibility: visible;
  opacity: 1;
  transition: opacity 0.35s ease, visibility 0s;
}
figure.sp .sp-fd {
  border: 1px solid var(--rule);
  background: var(--code-bg);
  padding: 0.2rem 0.55rem;
  visibility: hidden;
  opacity: 0;
  transition: opacity 0.35s ease, transform 0.35s ease, color 0.35s ease,
    border-color 0.35s ease, visibility 0s linear 0.35s;
}
figure.sp .p8,
figure.sp .p9 {
  transform: translateX(-1.5rem);
}
figure.sp .c8,
figure.sp .c9 {
  transform: translateX(1.5rem);
}
figure.sp .sp-end {
  color: var(--muted);
  white-space: nowrap;
  transition: color 0.25s ease;
}
figure.sp .sp-link {
  width: 1px;
  height: 1.5rem;
  background: var(--rule);
  transition: background 0.25s ease;
}
figure.sp #sp1:checked ~ .sp-grid .p8,
figure.sp #sp1:checked ~ .sp-grid .p9,
figure.sp #sp2:checked ~ .sp-grid .sp-fd,
figure.sp #sp3:checked ~ .sp-grid .sp-fd,
figure.sp #sp4:checked ~ .sp-grid .p9,
figure.sp #sp4:checked ~ .sp-grid .c8 {
  visibility: visible;
  opacity: 1;
  transform: none;
  transition: opacity 0.35s ease, transform 0.35s ease, color 0.35s ease,
    border-color 0.35s ease, visibility 0s;
}
figure.sp #sp3:checked ~ .sp-grid .p8,
figure.sp #sp3:checked ~ .sp-grid .c9 {
  color: var(--muted);
  text-decoration: line-through;
  opacity: 0.45;
}
figure.sp #sp4:checked ~ .sp-grid .p9,
figure.sp #sp4:checked ~ .sp-grid .c8 {
  color: var(--accent);
  border-color: var(--accent);
}
figure.sp #sp4:checked ~ .sp-grid .sp-end {
  color: var(--accent);
}
figure.sp #sp4:checked ~ .sp-grid .sp-link {
  background: var(--accent);
}
figure.sp figcaption {
  display: grid;
  margin-top: 1.25rem;
  color: var(--muted);
  font-size: 0.95rem;
  line-height: 1.5;
}
figure.sp figcaption p {
  grid-area: 1 / 1;
  margin: 0;
  visibility: hidden;
  opacity: 0;
  transform: translateY(4px);
  transition: opacity 0.25s ease, transform 0.25s ease,
    visibility 0s linear 0.25s;
}
figure.sp #sp1:checked ~ figcaption .c1,
figure.sp #sp2:checked ~ figcaption .c2,
figure.sp #sp3:checked ~ figcaption .c3,
figure.sp #sp4:checked ~ figcaption .c4 {
  visibility: visible;
  opacity: 1;
  transform: none;
  transition: opacity 0.25s ease 0.05s, transform 0.25s ease 0.05s,
    visibility 0s;
}
@media (prefers-reduced-motion: reduce) {
  figure.sp .sp-fd,
  figure.sp figcaption p {
    transform: none;
  }
}
</style>

<figure class="sp">
<input type="radio" name="sp" id="sp1" checked>
<input type="radio" name="sp" id="sp2">
<input type="radio" name="sp" id="sp3">
<input type="radio" name="sp" id="sp4">
<div class="sp-steps">
<label for="sp1">socketpair()</label>
<label for="sp2">clone()</label>
<label for="sp3">close()</label>
<label for="sp4">result</label>
</div>
<div class="sp-grid">
<span class="sp-head">bitcoin-node</span>
<span class="sp-head">socket pair</span>
<span class="sp-head child">bitcoin-wallet</span>
<span class="sp-fd p8">fd 8</span>
<span class="sp-end">&#8213; socket 0 &#8213;</span>
<span class="sp-fd c8">fd 8</span>
<span></span>
<span class="sp-link"></span>
<span></span>
<span class="sp-fd p9">fd 9</span>
<span class="sp-end">&#8213; socket 1 &#8213;</span>
<span class="sp-fd c9">fd 9</span>
</div>
<figcaption>
<p class="c1"><code>socketpair()</code> creates two already connected sockets. Both ends live in <code>bitcoin-node</code>, as fd 8 and fd 9.</p>
<p class="c2"><code>clone()</code> creates the child process, which gets a copy of the fd table. Now four fds point at the same two sockets. I call it <code>bitcoin-wallet</code> already, but it only becomes that at <code>execve</code>, which happens later.</p>
<p class="c3"><code>bitcoin-node</code> calls <code>close(8)</code> and the child calls <code>close(9)</code>. Each side drops the end it will not use.</p>
<p class="c4">One fd per side, and this is the pair we found earlier with <code>lsof</code>. Since each socket now has exactly one fd left, either socket closing makes the other side's read return EOF.</p>
</figcaption>
</figure>

<div class="with-aside">

That last part matters more than it looks. Because each socket now has a single descriptor keeping it open, either process dying closes its end for good, and the other side's next `read` returns EOF instead of hanging forever. The shutdown mechanism is built on exactly this: killing `bitcoin-node` makes `bitcoin-wallet` read EOF of the socket and fall out of `main`. [`serve()`](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/interfaces.cpp#L80), which is the only thing the spawned `bitcoin-wallet` has been doing since it started, returns. [`startSpawnedProcess` then sets `EXIT_SUCCESS`](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/interfaces.cpp#L81), [`MakeWalletInit` returns null](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/init/bitcoin-wallet-ipc.cpp#L89), and [`main` sees the null and returns that status](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/bitcoin-wallet.cpp#L101). No signal, nothing killed. `bitcoin-wallet` exits `0`, because as far as it is concerned it finished its job.

<aside class="side">
  <p>A spawned <code>bitcoin-wallet</code> <a href="https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/interfaces.cpp#L79">ignores Ctrl-C on purpose</a>. Ctrl-C goes to every process in the terminal's foreground group, so <code>bitcoin-wallet</code> gets it at the same moment <code>bitcoin-node</code> does, and only <code>bitcoin-node</code> should drive the shutdown.</p>
</aside>

</div>

Now that each process holds one fd, which fd do you read to get what the other side wrote? This trips people up, so follow one request and one reply through:

<style>
figure.io {
  position: relative;
  margin: 2rem 0;
  border: 1px solid var(--rule);
  padding: 1rem;
}
figure.io input {
  position: absolute;
  opacity: 0;
  pointer-events: none;
}
figure.io .io-steps {
  display: flex;
  flex-wrap: wrap;
  gap: 0.4rem;
  margin-bottom: 1.5rem;
}
figure.io .io-steps label {
  font-family: monospace;
  font-size: 0.8rem;
  line-height: 1;
  padding: 0.35rem 0.6rem;
  border: 1px solid var(--rule);
  color: var(--muted);
  cursor: pointer;
  transition: color 0.2s ease, background 0.2s ease, border-color 0.2s ease;
}
figure.io .io-steps label:hover {
  color: var(--accent);
  border-color: var(--accent);
}
figure.io #io1:focus-visible ~ .io-steps label[for="io1"],
figure.io #io2:focus-visible ~ .io-steps label[for="io2"],
figure.io #io3:focus-visible ~ .io-steps label[for="io3"],
figure.io #io4:focus-visible ~ .io-steps label[for="io4"] {
  outline: 2px solid var(--accent);
  outline-offset: 2px;
}
figure.io #io1:checked ~ .io-steps label[for="io1"],
figure.io #io2:checked ~ .io-steps label[for="io2"],
figure.io #io3:checked ~ .io-steps label[for="io3"],
figure.io #io4:checked ~ .io-steps label[for="io4"] {
  color: var(--bg);
  background: var(--fg);
  border-color: var(--fg);
}
figure.io .io-cols {
  display: grid;
  grid-template-columns: 1fr auto 1fr;
  align-items: start;
  gap: 0.75rem;
  font-family: monospace;
  font-size: 0.75rem;
  line-height: 1.4;
  color: var(--muted);
  margin: 0 0 0.75rem;
}
figure.io .io-cols span:nth-child(2) {
  align-self: end;
  text-align: center;
}
figure.io .io-cols span:last-child {
  text-align: right;
}
figure.io .io-stage {
  position: relative;
  width: 380px;
  height: 185px;
  margin: 0 auto;
}
@media (max-width: 460px) {
  figure.io .io-stage {
    width: 274px;
    height: 133px;
    transform: scale(0.72);
    transform-origin: top left;
  }
}
figure.io .io-stage svg {
  display: block;
  overflow: visible;
}
figure.io .io-wire {
  fill: none;
  stroke: var(--rule);
  stroke-width: 1.5;
  stroke-dasharray: 4 4;
  transition: stroke 0.3s ease;
}
figure.io .io-arrow {
  fill: var(--rule);
  transition: fill 0.3s ease;
}
figure.io .io-port {
  font-family: monospace;
  font-size: 12px;
  fill: var(--muted);
  transition: fill 0.3s ease;
}
figure.io .io-pkt {
  position: absolute;
  top: 0;
  left: 0;
  padding: 0.15rem 0.5rem;
  font-size: 0.75rem;
  border: 1px solid var(--accent);
  background: var(--code-bg);
  color: var(--accent);
  white-space: nowrap;
  offset-rotate: 0deg;
  offset-distance: 0%;
  visibility: hidden;
  opacity: 0;
  transition: offset-distance 0.6s ease, opacity 0.3s ease,
    visibility 0s linear 0.3s;
}
figure.io .pkt-a {
  offset-path: path("M 25 36 C 165 36, 205 151, 340 151");
}
figure.io .pkt-b {
  offset-path: path("M 355 36 C 215 36, 175 151, 40 151");
}
figure.io #io1:checked ~ .io-stage .pkt-a,
figure.io #io2:checked ~ .io-stage .pkt-a,
figure.io #io3:checked ~ .io-stage .pkt-b,
figure.io #io4:checked ~ .io-stage .pkt-b {
  visibility: visible;
  opacity: 1;
  transition: offset-distance 0.6s ease, opacity 0.3s ease, visibility 0s;
}
figure.io #io2:checked ~ .io-stage .pkt-a,
figure.io #io4:checked ~ .io-stage .pkt-b {
  offset-distance: 100%;
}
figure.io #io1:checked ~ .io-stage .c-w9,
figure.io #io2:checked ~ .io-stage .c-r8,
figure.io #io3:checked ~ .io-stage .c-w8,
figure.io #io4:checked ~ .io-stage .c-r9 {
  fill: var(--accent);
}
figure.io #io1:checked ~ .io-stage .wire-a,
figure.io #io2:checked ~ .io-stage .wire-a,
figure.io #io3:checked ~ .io-stage .wire-b,
figure.io #io4:checked ~ .io-stage .wire-b {
  stroke: var(--accent);
}
figure.io #io1:checked ~ .io-stage .arrow-a,
figure.io #io2:checked ~ .io-stage .arrow-a,
figure.io #io3:checked ~ .io-stage .arrow-b,
figure.io #io4:checked ~ .io-stage .arrow-b {
  fill: var(--accent);
}
figure.io figcaption {
  display: grid;
  margin-top: 1.25rem;
  color: var(--muted);
  font-size: 0.95rem;
  line-height: 1.5;
}
figure.io figcaption p {
  grid-area: 1 / 1;
  margin: 0;
  visibility: hidden;
  opacity: 0;
  transform: translateY(4px);
  transition: opacity 0.25s ease, transform 0.25s ease,
    visibility 0s linear 0.25s;
}
figure.io #io1:checked ~ figcaption .k1,
figure.io #io2:checked ~ figcaption .k2,
figure.io #io3:checked ~ figcaption .k3,
figure.io #io4:checked ~ figcaption .k4 {
  visibility: visible;
  opacity: 1;
  transform: none;
  transition: opacity 0.25s ease 0.05s, transform 0.25s ease 0.05s,
    visibility 0s;
}
@media (prefers-reduced-motion: reduce) {
  figure.io .io-pkt,
  figure.io figcaption p {
    transition-property: opacity, visibility;
  }
}
</style>

<figure class="io">
<input type="radio" name="io" id="io1" checked>
<input type="radio" name="io" id="io2">
<input type="radio" name="io" id="io3">
<input type="radio" name="io" id="io4">
<div class="io-steps">
<label for="io1">write(9)</label>
<label for="io2">read(8)</label>
<label for="io3">write(8)</label>
<label for="io4">read(9)</label>
</div>
<div class="io-cols">
<span>bitcoin-node<br>fd 9, socket 1</span>
<span>bytes on the wire</span>
<span>bitcoin-wallet<br>fd 8, socket 0</span>
</div>
<div class="io-stage">
<svg viewBox="0 0 380 185" width="380" height="185" aria-hidden="true">
<path class="io-wire wire-a" d="M 25 36 C 165 36, 205 151, 340 151"></path>
<polygon class="io-arrow arrow-a" points="340,144 355,151 340,158"></polygon>
<path class="io-wire wire-b" d="M 355 36 C 215 36, 175 151, 40 151"></path>
<polygon class="io-arrow arrow-b" points="40,144 25,151 40,158"></polygon>
<text class="io-port c-w9" x="0" y="18" text-anchor="start">write(9)</text>
<text class="io-port c-w8" x="380" y="18" text-anchor="end">write(8)</text>
<text class="io-port c-r9" x="0" y="180" text-anchor="start">read(9)</text>
<text class="io-port c-r8" x="380" y="180" text-anchor="end">read(8)</text>
</svg>
<span class="io-pkt pkt-a">request</span>
<span class="io-pkt pkt-b">reply</span>
</div>
<figcaption>
<p class="k1"><code>bitcoin-node</code> writes a request on fd 9. The bytes never sit in socket 1, the kernel hands them straight to the peer.</p>
<p class="k2">They land in socket 0's receive queue, so <code>bitcoin-wallet</code> gets them by reading its own fd 8. Written on 9, read on 8.</p>
<p class="k3">The reply goes out the same way, <code>bitcoin-wallet</code> writes it on fd 8 and it crosses to socket 1.</p>
<p class="k4"><code>bitcoin-node</code> reads its own fd 9 and gets what <code>bitcoin-wallet</code> wrote on fd 8. You always read your own fd, and what comes out is whatever the peer wrote.</p>
</figcaption>
</figure>

Ok, so until now we followed through the `bitcoin-node` main thread syscalls showing that it creates fd 9 to speak with `bitcoin-wallet`, so its trace should be full of `write(9)` and `read(9)`, right? But if we grep for these on the `bitcoin-node` main thread trace we won't find anything, not a single one.

The reason is that the main thread is not the one doing the reading and writing. Look at the trace after `close(8)`:

```bash
11:53:22.283278 clone3({flags=CLONE_VM|CLONE_FS|CLONE_FILES|CLONE_SIGHAND|CLONE_THREAD|...}) = 72394 <0.000038>
```

Here [`clone3`](https://man7.org/linux/man-pages/man2/clone3.2.html) syscall is creating a new thread and we can see it returning the TID 72394, so this is the thread we should look into. But where does it come from?

After `mp::SpawnProcess` the main thread calls [`CapnpProtocol.connect`](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/capnp/protocol.cpp#L80) with the fd 9 and exe_name "bitcoin-wallet":

```c++
   std::unique_ptr<interfaces::Init> connect(int fd, const char* exe_name) override
    {
        startLoop(exe_name);
        return mp::ConnectStream<messages::Init>(*m_loop, fd);
    }
```

Which calls [`CapnpProtocol.startLoop`](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/capnp/protocol.cpp#L123):

```c++
    void startLoop(const char* exe_name)
    {
        if (m_loop) return;
        std::promise<void> promise;
        m_loop_thread = std::thread([&] {
            util::ThreadRename("capnp-loop");
            mp::LogOptions opts = {
                .log_fn = IpcLogFn,
                .log_level = GetRequestedIPCLogLevel()
            };
            m_loop.emplace(exe_name, std::move(opts), &m_context);
            m_loop_ref.emplace(*m_loop);
            promise.set_value();
            m_loop->loop();
            m_loop.reset();
        });
        promise.get_future().wait();
    }
```

There is our thread. `std::thread` is the C++ way of asking for one, and on Linux that becomes the `clone3` we just saw in the trace. The lambda it runs renames itself to `capnp-loop`, and since [`util::ThreadRename` prepends `b-`](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/util/threadnames.cpp#L58) that is the `b-capnp-loop` we found in `ps`.

What it runs is an [`mp::EventLoop`](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/libmultiprocess/include/mp/proxy-io.h#L241), which is responsible for sending and receiving capnp messages: one thread that parks on a whole set of file descriptors at once and, whenever any of them becomes ready, runs the callback waiting on it. That is how a single thread services many in-flight calls without blocking on any one of them, and it is where the `write(9)` and `read(9)` we are after will show up.

Note the order in `CapnpProtocol.connect`: `CapnpProtocol.startLoop` first, then `mp::ConnectStream`. The event loop thread has to exist before there is anything to hand a connection to. `ConnectStream` is the function that wraps fd 9 in a capnp connection and asks for the bootstrap capability, and it is the one that emits the very first message on the wire.

### Tracing capnp Event Loop Thread

Let's trace the event loop thread. Its file is `tracing.log.72394`, and we can see it rename itself:

```bash
11:53:22.283447 prctl(PR_SET_NAME, "b-capnp-loop") = 0 <0.000006>
```

Right after that it builds the `mp::EventLoop` object, and this is its [constructor](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/libmultiprocess/src/mp/proxy.cpp#L202):

```c++
EventLoop::EventLoop(const char* exe_name, LogOptions log_opts, void* context)
    : m_exe_name(exe_name),
      m_io_context(kj::setupAsyncIo()),
      m_task_set(new kj::TaskSet(m_error_handler)),
      m_log_opts(std::move(log_opts)),
      m_context(context)
{
    int fds[2];
    KJ_SYSCALL(socketpair(AF_UNIX, SOCK_STREAM, 0, fds));
    m_wait_fd = fds[0];
    m_post_fd = fds[1];
}
```

Two lines in there make interesting syscalls.

The first is `kj::setupAsyncIo()` in the initializer list.

KJ is Cap'n Proto's own C++ toolkit library, written by the same author and shipped in the same repo. Among other things it provides the promise type and the single-threaded event loop that [Cap'n Proto RPC is built on](https://capnproto.org/cxxrpc.html#kj-concurrency-framework), so using capnp means using KJ.

`mp::EventLoop` uses that KJ event loop behind the scenes, and we can see [KJ setting up epoll and an eventfd](https://github.com/capnproto/capnproto/blob/v1.2.0/c%2B%2B/src/kj/async-unix.c%2B%2B#L412):

```bash
11:53:22.283602 epoll_create1(EPOLL_CLOEXEC) = 8<anon_inode:[eventpoll]> <0.000013>
11:53:22.283642 eventfd2(0, EFD_CLOEXEC|EFD_NONBLOCK) = 10<{eventfd-count=0, eventfd-id=85, eventfd-semaphore=0}> <0.000006>
```

The second is the body, where `mp::EventLoop` creates its own internal socket pair and keeps one end as `m_wait_fd` and the other as `m_post_fd`:

```bash
11:53:22.283759 socketpair(AF_UNIX, SOCK_STREAM, 0, [11<UNIX-STREAM:[508632->508633]>, 12<UNIX-STREAM:[508633->508632]>]) = 0 <0.000013>
```

This is the second socketpair we have seen, and it has nothing to do with the socketpair used for IPC communication. It serves as a wakeup channel, and it exists because there is no way to hand work to a thread that is parked in `epoll_wait`: the thread is not executing, and the kernel will only wake it for I/O. So libmultiprocess makes the handoff look like I/O. A thread calling [`EventLoop::post()`](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/libmultiprocess/src/mp/proxy.cpp#L279) leaves its callback where the event loop can find it and writes a single byte to `m_post_fd`. The byte carries no data, it only wakes the loop.

This matters here because IPC calls are made from arbitrary Bitcoin Core threads, but only `b-capnp-loop` may touch the connection, [because capnp assumes it will only be accessed from a single thread](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/libmultiprocess/include/mp/proxy-io.h#L218).

Which also settles the three sockets `lsof` showed us at the start. fd 11 and fd 12 are this wakeup channel, private to the process. fd 9 is the only one that ever talks to `bitcoin-wallet`.

Then it calls [`m_loop->loop()`](https://github.com/bitcoin/bitcoin/blob/b314fb2cff7ceb68882f9e53294f86dab6940337/src/ipc/libmultiprocess/src/mp/proxy.cpp#L230), this is the core of the event loop, trimmed to the parts that matter here:

```c++
void EventLoop::loop()
{
    // ...

    kj::Own<kj::AsyncIoStream> wait_stream{
        m_io_context.lowLevelProvider->wrapSocketFd(m_wait_fd, kj::LowLevelAsyncIoProvider::TAKE_OWNERSHIP)};
    char buffer = 0;
    for (;;) {
        const size_t read_bytes = wait_stream->read(&buffer, 0, 1).wait(m_io_context.waitScope);
        if (read_bytes != 1) throw std::logic_error("EventLoop wait_stream closed unexpectedly");
        Lock lock(m_mutex);
        if (m_post_fn) {
            Unlock(lock, *m_post_fn);
            m_post_fn = nullptr;
            m_cv.notify_all();
        } else if (done()) {
            break;
        }
    }

    // ...
}
```

As the name implies, an event loop has a literal loop inside it. Notice the `for (;;)`, an infinite loop that only ever leaves through the `break`.

Before entering it, the event loop thread calls `wrapSocketFd(m_wait_fd, TAKE_OWNERSHIP)`, which wraps `m_wait_fd` in a KJ asynchronous interface. In the trace that wrapping is three syscalls:

```bash
11:53:22.283901 ioctl(11<UNIX-STREAM:[508632->508633]>, FIONBIO, [1]) = 0 <0.000022>
11:53:22.283959 ioctl(11<UNIX-STREAM:[508632->508633]>, FIOCLEX) = 0 <0.000010>
11:53:22.283988 epoll_ctl(8<anon_inode:[eventpoll]>, EPOLL_CTL_ADD, 11<UNIX-STREAM:[508632->508633]>, {events=EPOLLIN|EPOLLOUT|EPOLLRDHUP|EPOLLET, data=0x7f1564000c80}) = 0 <0.000010>
```

[`ioctl`](https://man7.org/linux/man-pages/man2/ioctl.2.html) sets `FIONBIO` and `FIOCLEX` on `m_wait_fd` flags. `FIONBIO` makes fd 11 non blocking, which is the rule the whole design rests on: a thread serving many descriptors can never afford to block on any one of them. `FIOCLEX` sets close-on-exec, so the descriptor does not leak into child processes. And [`epoll_ctl`](https://man7.org/linux/man-pages/man2/epoll_ctl.2.html) registers fd 11 with the epoll instance on fd 8 created back in the constructor, so from here on the kernel does the watching and reports when those events happen.

Then it enters `for (;;)` and calls `wait_stream->read(&buffer, 0, 1).wait(m_io_context.waitScope)`, which will `read(11)` directly if there is any content or it will wait with [`epoll_wait`](https://man7.org/linux/man-pages/man2/epoll_wait.2.html) for any event registered with `epoll_ctl`.

This is it for the event loop on startup. `CapnpProtocol.connect` calls `CapnpProtocol.startLoop`, which spawns `b-capnp-loop` and then parks on `promise.get_future().wait()`, because the very next thing it wants to do is `mp::ConnectStream<messages::Init>(*m_loop, fd)`, and that dereferences an `m_loop` that was built on the other thread. The event loop thread builds it, epoll on fd 8, an eventfd on fd 10, the wakeup pair on fds 11 and 12, and `promise.set_value()` releases the main thread.

So here is the shape of every IPC call in Bitcoin Core, and it is not the shape you would guess from the call site. A thread in `bitcoin-node` writes `init->makeWalletLoader(...)`, which looks like a function call and behaves like one. Underneath, that thread stores its callback in `m_post_fn`, writes a single meaningless byte to fd 12 and goes to sleep. `b-capnp-loop` wakes up, and it is the one that reads `m_post_fn` and makes the request on the actual socket on fd 9. The thread that made the call never touches the socket, which is why grepping its trace for `write(9)` or `read(9)` comes back empty.

That is the machinery. In the [next post](https://viniciuscestari.dev/posts/bitcoin-core-ipc-on-the-wire) we finally read what it carries: the bootstrap, `Init.construct`, `ThreadMap.makeThread`, and the `Init.makeWalletLoader` call that the first post predicted, decoded byte by byte with capnp's own schema.
