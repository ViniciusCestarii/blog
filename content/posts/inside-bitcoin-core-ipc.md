---
title: Inside Bitcoin Core IPC
date: 2026-07-23
toc: true
released: false
---

In [my last post](https://viniciuscestari.dev/posts/introduction-to-bitcoin-core-ipc) I have made an introduction to Bitcoin Core IPC mechanism and why it is important. In this one I want to dive deeper and show this mechanism working in practice between `bitcoin-node` and `bitcoin-wallet` binaries.

## Setting Up

We will need to use the changes on the [PR 10102 Multiprocess bitcoin (this is the oldest open PR in Bitcoin Core!)](https://github.com/bitcoin/bitcoin/pull/10102) to try the split of bitcoin-node and bitcoin-wallet.

### Building Bitcoin Core IPC

<div class="with-aside">

Building it is pretty straithforward because the default build already builds multiprocess build.

```bash
cmake -B build
cmake --build build -j $(nproc)
```

This may take a while but after running this you should see some binaries in folder `./build/bin/` including `bitcoin`, `bitcoin-node` and `bitcoin-wallet`.

<aside class="side">
  <p>Sorry, I will only use Unix commands here this post will be Linux heavy. <a href="https://github.com/bitcoin/bitcoin/pull/32387">Windows support for this build is still in development. </a></p>
</aside>

</div>

## Running Multiprocess Bitcoin Core

<div class="with-aside">

Using the `bitcoin` CLI we can start `bitcoin-node`:

```bash
build/bin/bitcoin -m node -debug=ipc -regtest
```

Here we use `-m` flag to state that we want to run the multiprocess `bitcoin-node` and not `bitcoind` and `-debug=ipc` to print IPC logs. 

<aside class="side">
  <p><code>bitcoin</code> is just a CLI helper to run other commands. To see more, check <a href="https://github.com/bitcoin/bitcoin/blob/master/src/bitcoin.cpp">src/bitcoin.cpp</a>.</p>
</aside>

</div>

Ok now with `bitcoin-node` running let's run `bitcoin-wallet`, but wait let's first run this comamnd to see our `bitcoin-node` running:

```bash
pstree | grep bitcoin
```

should be a aside: I will be using ps commands a lot to inspect process in this blog post

result:
```bash
vinicius@archlinux ~> pstree | grep bitcoin
        |         |-alacritty-+-fish---bitcoin-node-+-bitcoin-wallet---2*[{bitcoin-wallet}]
        |         |           |                     `-48*[{bitcoin-node}]
```
The numbers are just the number of OS threads of that process.


What does this mean? Well this means that in my case I ran the terminal emulator [Alacritty](https://alacritty.org/) which runs the [fish shell](https://fishshell.com/) I ran `build/bin/bitcoin -m node -debug=ipc -regtest` and `bitcoin` then called `execvp` with `./build/bin/bitcoin-node` replacing the `bitcoin` process image with a `bitcoin-node` process image (that's why we don't see a `bitcoin` binary on the comamnd output) and also the interesting part: `bitcoin-node` spawned `bitcoin-wallet`, this preserves monolith-equivalent UX (bitcoin -m node "just has a wallet" like bitcoind does) without you manually starting and wiring up bitcoin-wallet yourself.

Also running to see the pid and command of current process that mention "bitcoin":
```bash
ps a -o pid,cmd | grep bitcoin
```

result:
```bash
vinicius@archlinux ~> ps a -o pid,cmd | grep bitcoin
  63353 /home/vinicius/Code/my/bitcoin/build/bin/bitcoin-node -debug=ipc -regtest
  63355 /home/vinicius/Code/my/bitcoin/build/bin/bitcoin-wallet -ipcfd 8
```

Here we see the `bitcoin-node` args are what we passed to `bitcoin` and also `bitcoin-wallet` with the flag `-ipcfd 8`. This is the [file descriptor](https://en.wikipedia.org/wiki/File_descriptor) of one end of a Unix Domain socket that the parent `bitcoin-node` sets up before creating the child and then pass one end for the it via this arg. 

When `bitcoin-wallet` starts it checks for `-ipcfd` and it is set it branch out to an IPC server and just answers and waits for `bitcoin-nodes` (to understand more follow the function `interfaces::MakeWalletInit` in [src/bitcoin-wallet.cpp](https://github.com/ryanofsky/bitcoin/blob/pr/ipc/src/bitcoin-wallet.cpp#L100) ). 


## Tracing IPC Communication



First let's stop `bitcoin-node` (this will stop `bitcoin-wallet` since `bitcoin-wallet` will notice that the unix domain socket is dead and [will exit normally](https://github.com/ryanofsky/bitcoin/blob/pr/ipc/src/bitcoin-wallet.cpp#L102)) so we can start tracing from the beginning:

```bash
kill $(pidof bitcoin-node)
```

Now that `bitcoin-node` and `bitcoin-wallet` are down let's run again and trace it with `strace`!

```bash
mkdir trace
strace -ff -o trace/tracing.log -tt -T -yy -s 999999 build/bin/bitcoin -m node -debug=ipc -regtest
```

The flags of `strace` set here are:

| Flag | Meaning |
|---|---|
| `-ff` | Follow forks, writing each traced process's output to a separate file |
| `-o trace/tracing.log` | Send trace output to file, with `-ff` output to file appended by TID (thread id) |
| `-tt` | Print absolute timestamp with microsecond precision |
| `-T` | Print time spent in each syscall |
| `-yy` | Print all available info for file descriptor arguments |
| `-s 999999` | Limit length of printed strings (raised high to avoid truncation) |

Fun fact: tracing bitcoin core like this can take a huge amount of storage and make the largest files I have ever seen.

Now if we run `ls trace` to see what files there are in our folder trace we will see:

```bash
vinicius@archlinux ~> ls trace
tracing.log.21307  tracing.log.21432  tracing.log.21438  tracing.log.21444  tracing.log.21450  tracing.log.21456  tracing.log.21462  tracing.log.21468  tracing.log.21474
tracing.log.21308  tracing.log.21433  tracing.log.21439  tracing.log.21445  tracing.log.21451  tracing.log.21457  tracing.log.21463  tracing.log.21469  tracing.log.21475
tracing.log.21309  tracing.log.21434  tracing.log.21440  tracing.log.21446  tracing.log.21452  tracing.log.21458  tracing.log.21464  tracing.log.21470  tracing.log.21476
tracing.log.21310  tracing.log.21435  tracing.log.21441  tracing.log.21447  tracing.log.21453  tracing.log.21459  tracing.log.21465  tracing.log.21471  tracing.log.21477
tracing.log.21428  tracing.log.21436  tracing.log.21442  tracing.log.21448  tracing.log.21454  tracing.log.21460  tracing.log.21466  tracing.log.21472  tracing.log.21478
tracing.log.21431  tracing.log.21437  tracing.log.21443  tracing.log.21449  tracing.log.21455  tracing.log.21461  tracing.log.21467  tracing.log.21473  tracing.log.21479
```

Ok this is a lot and these are the `bitcoin-node` and `bitcoin-wallet` tracing combined and only differentiated by TID. So now we need to figure out what what thread belong to who.

This is easy we can run the following to get all the current `bitcoin-node` threads TID and respectvive command name:

```bash
ps -T -p $(pidof bitcoin-node) -o tid,comm
```

```bash
vinicius@archlinux ~> ps -T -p $(pidof bitcoin-node) -o tid,comm
    TID COMMAND
  21307 bitcoin-node
  21308 b-scheduler
  21310 b-capnp-loop
  21431 b-http.00
  21432 b-http.01
  21433 b-http.02
  21434 b-http.03
  21435 b-http.04
  21436 b-http.05
  21437 b-http.06
  21438 b-http.07
  21439 b-http.08
  21440 b-http.09
  21441 b-http.10
  21442 b-http.11
  21443 b-http.12
  21444 b-http.13
  21445 b-http.14
  21446 b-http.15
  21447 b-http
  21448 b-scriptch.00
  21449 b-scriptch.01
  21450 b-scriptch.02
  21451 b-scriptch.03
  21452 b-scriptch.04
  21453 b-scriptch.05
  21454 b-scriptch.06
  21455 b-scriptch.07
  21456 b-scriptch.08
  21457 b-scriptch.09
  21458 b-scriptch.10
  21459 b-scriptch.11
  21460 b-scriptch.12
  21461 b-scriptch.13
  21462 b-scriptch.14
  21463 b-prevout.00
  21464 b-prevout.01
  21465 b-prevout.02
  21466 b-prevout.03
  21467 b-prevout.04
  21468 b-prevout.05
  21469 b-prevout.06
  21470 b-prevout.07
  21472 b-mapport
  21473 b-torcontrol
  21474 b-net
  21476 b-addcon
  21477 b-opencon
  21478 b-msghand
```

There are two interesting things here, one is noticed that every thread besides `bitcoin-node` starts with `b-` this is a convention in Bitcoin Core where every thread created is preffixed with `b-`, so why `bitcoin-node` doesn't start with `b-`? It doesn't because the `bitcoin-node` thread is the main thread, it was first thread created ( lowest TID) and was created with the process and the main thread always take the program name.

Let's find out threads TID of `bitcoin-wallet` too:

```bash
vinicius@archlinux ~> ps -T -p $(pidof bitcoin-wallet) -o tid,comm
    TID COMMAND
  21309 bitcoin-wallet
  21428 bitcoin-wallet
  21479 b-schedqueue
```

Ok so we found out all threads!

aside to add lather: there are two `bitcoin-wallet` thread names because when a process creates a bew thread and doens't rename it creates with the same name as the thread that created it.

--- later in this post:

Let's give strace something to chew on.


```bash
build/bin/bitcoin-cli -regtest createwallet hello
```
now if we see the number of threads again:

```bash
vinicius@archlinux ~> ps -T -p $(pidof bitcoin-node) -o tid,comm
    TID COMMAND
  25846 bitcoin-node
  25847 b-scheduler
  25849 b-capnp-loop
  25932 b-http.00
  25933 b-http.01
  25934 b-http.02
  25935 b-http.03
  25936 b-http.04
  25937 b-http.05
  25938 b-http.06
  25939 b-http.07
  25940 b-http.08
  25941 b-http.09
  25942 b-http.10
  25943 b-http.11
  25944 b-http.12
  25945 b-http.13
  25946 b-http.14
  25947 b-http.15
  25948 b-http
  25949 b-scriptch.00
  25950 b-scriptch.01
  25951 b-scriptch.02
  25952 b-scriptch.03
  25953 b-scriptch.04
  25954 b-scriptch.05
  25955 b-scriptch.06
  25956 b-scriptch.07
  25957 b-scriptch.08
  25958 b-scriptch.09
  25959 b-scriptch.10
  25960 b-scriptch.11
  25961 b-scriptch.12
  25962 b-scriptch.13
  25963 b-scriptch.14
  25964 bitcoin-node
  25965 b-prevout.00
  25966 b-prevout.01
  25967 b-prevout.02
  25968 b-prevout.03
  25969 b-prevout.04
  25970 b-prevout.05
  25971 b-prevout.06
  25972 b-prevout.07
  25974 b-mapport
  25975 b-torcontrol
  25976 b-net
  25978 b-addcon
  25979 b-opencon
  25980 b-msghand
  26409 b-capnp-loop # this is new! Also crop the majority of these since they arent new
```

```bash
vinicius@archlinux ~> ps -T -p $(pidof bitcoin-wallet) -o tid,comm
    TID COMMAND
  25848 bitcoin-wallet
  25922 bitcoin-wallet
  25981 b-schedqueue
  26024 bitcoin-wallet # this is new!
```