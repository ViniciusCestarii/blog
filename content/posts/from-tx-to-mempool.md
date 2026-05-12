---
title: From TX message to mempool
date: 2026-05-12
---

This is a walk through what Bitcoin Core v31.0 does when a peer sends us a `TX` message. I am tracing the path inside [`net_processing.cpp`](https://github.com/bitcoin/bitcoin/blob/v31.0/src/net_processing.cpp#L4473), from the moment we process `TX` message until it either lands in the mempool or gets rejected.

<div class="with-aside">

A `TX` message normally arrives as the response to a `GETDATA` we sent earlier, and only rarely as an unsolicited announcement. Either way, the node runs through the same sequence of checks. Let's follow that path.

<aside class="side">
  <p>Code snippets are reformatted to fit this blog size. The only edits to the original Bitcoin Core source are shortened log calls and comments. Logic is untouched.</p>
</aside>

</div>

## Should we even listen to this peer?

Before looking at the transaction itself, the node decides whether it should consider a `TX` message from this peer at all calling `RejectIncomingTxs`. The three cases where Core ignores the `TX` message and marks peer for disconnection (`pfrom.fDisconnect = true;`):

```cpp
bool PeerManagerImpl::RejectIncomingTxs(const CNode& peer) const
{
    // block-relay-only peers may never send txs to us
    if (peer.IsBlockOnlyConn()) return true;
    if (peer.IsFeelerConn()) return true;
    // In -blocksonly mode, peers need the 'relay' 
    // permission to send txs to us
    if (m_opts.ignore_incoming_txs && 
          !peer.HasPermission(NetPermissionFlags::Relay)) return true;
    return false;
}
```

1. `peer.IsBlockOnlyConn()`: this connection exists for blocks, not for transaction relay.
2. `peer.IsFeelerConn()`: feeler connections are short-lived connections to check that a node is alive, not meant for data exchange.
3. `m_opts.ignore_incoming_txs && !peer.HasPermission(NetPermissionFlags::Relay)` Our node started in `-blocksonly` mode and the peer lacks the relay permission, an explicit operator-granted permission.

If we are still doing IBD (Initial Block Download), we just ignore the message because we don't have enough information to validate it while we are still catching up to the tip and we don't mark for disconnection since this isn't a violation.

```cpp
if (m_chainman.IsInitialBlockDownload()) return;
```

## Deserialize and identify the transaction

If we decide to proceed, the payload is deserialized into a `CTransactionRef`:

```cpp
CTransactionRef ptx;
vRecv >> TX_WITH_WITNESS(ptx);

const Txid& txid = ptx->GetHash();
const Wtxid& wtxid = ptx->GetWitnessHash();

const uint256& hash = peer.m_wtxid_relay 
                                    ? wtxid.ToUint256() 
                                    : txid.ToUint256();
AddKnownTx(peer, hash);
```

Bitcoin has two transaction identifiers: `txid` (a hash of the transaction data without the witness) and `wtxid` (a hash that includes the witness, introduced with SegWit). Which one we use with a peer depends on whether they signaled `wtxidrelay` during the handshake, if they did, `peer.m_wtxid_relay` is true and we use `wtxid`; otherwise we fall back to `txid`.

`AddKnownTx` records that this peer is aware of this transaction. This prevents us from relaying it back to them later.

## Was this our own private broadcast returning home?

The next step is a check for whether this transaction is one we recently tried to privately broadcast.

<div class="with-aside">

Private broadcast is a new v31.0 Bitcoin Core feature that improves submission privacy. Normally, when you submit a transaction via `sendrawtransaction` RPC, it enters your local mempool first which means anyone watching your node can associate you with that transaction. When the node is started with the `-privatebroadcast` daemon flag, transactions sent via the `sendrawtransaction` RPC are pushed out over short-lived Tor or I2P connections without first being added to the local mempool, making it harder to link back to you.

<aside class="side">
  <p>Transactions submitted through the wallet are not affected by this option.</p>
</aside>

</div>

If we recently used the private broadcast feature to push a transaction out through a few short-lived connections, seeing it come back from the network is good news: it means at least one of those attempts succeeded and the transaction is propagating. We can stop trying the remaining outbound attempts.

```cpp
if (const auto num_broadcasted{
        m_tx_for_private_broadcast.Remove(ptx)
    }) {
    LogDebug(
      BCLog::PRIVBROADCAST, 
      "Received our privately broadcast transaction ..."
    );
    if (NUM_PRIVATE_BROADCAST_PER_TX > num_broadcasted.value()) {
        m_connman.m_private_broadcast.NumToOpenSub(
          NUM_PRIVATE_BROADCAST_PER_TX - num_broadcasted.value()
        );
    }
}
```

If not all the planned private connections were opened yet, we tell `CConnman` to cancel the remainder. No need to burn connections on a transaction the network already has.

## Two concepts you'll need

**CPFP** (Child-Pays-For-Parent): When a transaction sits unconfirmed because its fee is too low for miners to bother with, the recipient can create a second transaction that spends one of those outputs, even though the first transaction hasn't confirmed yet. This child transaction carries a high enough fee that miners, who must include the parent first in order to make the child valid, find the combined fee rate of the pair attractive enough to mine both.

<div class="with-aside">

**1P1C** (One-Parent-One-Child): Bitcoin Core's current implementation of CPFP in the mempool acceptance layer handles the simplest topology: exactly one parent paired with one child, evaluated together on their combined fee rate. When Core receives a transaction it would normally reject for a fee-related reason, it checks the orphanage for a child from the same peer that spends it. If found, it submits the pair as a package. This is the mechanism behind the `Find1P1CPackage` calls you see throughout the code below.

<aside class="side">
  <p>Restricting the search to the same peer stops an attacker from censoring the real child by flooding fake children for the same parent. See <a href="https://github.com/bitcoin/bitcoin/blob/v31.0/src/node/txdownloadman_impl.cpp#L303">txdownloadman_impl.cpp:303-307</a>.</p>
</aside>
</div>

## Ask the download manager what to do next

Now the transaction download manager gets a turn:

```cpp
LOCK2(cs_main, m_tx_download_mutex);

const auto& [should_validate, package_to_validate] =
    m_txdownloadman.ReceivedTx(pfrom.GetId(), ptx);
```

`ReceivedTx` does two things: it clears the in-flight slot we were holding for this (peer, txhash) pair in the `TxRequestTracker`, and it tells us what to do next. Three outcomes:

```c++
std::pair<bool, std::optional<PackageToValidate>> 
  TxDownloadManagerImpl::ReceivedTx(
    NodeId nodeid, 
    const CTransactionRef& ptx
  )
{
    const Txid& txid = ptx->GetHash();
    const Wtxid& wtxid = ptx->GetWitnessHash();

    m_txrequest.ReceivedResponse(nodeid, txid.ToUint256());
    if (ptx->HasWitness()) 
        m_txrequest.ReceivedResponse(nodeid, wtxid.ToUint256());

    if (AlreadyHaveTx(wtxid, /*include_reconsiderable=*/false)) {
        return {false, std::nullopt};
    } else if (RecentRejectsReconsiderableFilter()
                                      .contains(wtxid.ToUint256())) {
        LogDebug(BCLog::TXPACKAGES,
          "found tx in reconsiderable rejects, "
          "looking for child in orphanage...");
        return {false, Find1P1CPackage(ptx, nodeid)};
    }
    return {true, std::nullopt};
}
```

<div class="with-aside">

- **Already seen** (`AlreadyHaveTx` returns `true`): The transaction is already in our ophanage, mempool, recent confirmed transaction filter or recent rejected transaction filter, so nothing to validate.
- **Rejected recently for a reconsiderable reason** (the wtxid is in `RecentRejectsReconsiderableFilter`): We rejected this transaction recently for some policy reason (e.g. low fee), but it might be acceptable if validated as a 1P1C (one-parent-one-child) package.
- **Never seen it**: We will validate it.

<aside class="side">
  <p><em>recent rejected transaction filter</em> and the <em>recent rejects reconsiderable filter</em> are different things.</p>
</aside>
</div>

## Handle already-known and reconsidered transactions

If `should_validate` is false, two things can still happen before we return.

**Force relay**: If the peer has `NetPermissionFlags::ForceRelay`, an explicit operator-granted permission, we re-announce the transaction to all our peers even though we already had it, as long as it is still in our mempool. Its real effect is a rebroadcast amplifier, useful when a downstream node behind a trusted relay wants to retrigger propagation for a tx that is languishing in mempools without reaching miners.

**Package validation**: If `ReceivedTx` returned a 1P1C package, we attempt to validate that pair together. [`ProcessNewPackage`](https://github.com/bitcoin/bitcoin/blob/v31.0/src/validation.cpp#L1804) does the heavy work here: it runs the full package validation pipeline and, on success, adds the transactions to the mempool. I'm not going to walk through that phase in detail because it would be a blog post of its own.

<div class="with-aside">

```cpp
if (!should_validate) {
  if (pfrom.HasPermission(NetPermissionFlags::ForceRelay)) {
      if (!m_mempool.exists(txid)) {
          LogInfo("Not relaying non-mempool transaction...");
      } else {
          LogInfo("Force relaying tx %s (wtxid=%s) from peer...");
          InitiateTxBroadcastToAll(txid, wtxid);
      }
  }

  if (package_to_validate) {
      const auto package_result{
        ProcessNewPackage(
          m_chainman.ActiveChainstate(), 
          m_mempool, package_to_validate->m_txns, 
          /*test_accept=*/false, 
          /*client_maxfeerate=*/std::nullopt
          )
        };
      ProcessPackageResult(
        package_to_validate.value(), package_result
      );
  }
  // Nothing more to do with this transaction
  return;
}
```

<aside class="side">
  <p><code>InitiateTxBroadcastToAll</code> queues the tx for announcement to every relay-enabled peer in their next <code>INV</code> message.</p>
</aside>

</div>

If the package validation is attempted, two outcomes:

- Valid package: every transaction is already in the mempool at this point. Each is then passed through `ProcessValidTx`, which handles the post-acceptance bookkeeping and queues it for broadcast via `InitiateTxBroadcastToAll`.
- Invalid package: we add the package hash to `RecentRejectsReconsiderableFilter` so we do not re-evaluate the same parent+child pairing.

## The main validation path

This is the main path: a transaction we have not seen, ready for verification. [`m_chainman.ProcessTransaction`](https://github.com/bitcoin/bitcoin/blob/v31.0/src/validation.cpp#L4480) does the heavy work here: it runs the full transaction validation pipeline and, if everything checks out, adds the transaction to the mempool. I'm not going to walk through that phase in detail because it would be a blog post of its own.

```cpp
const MempoolAcceptResult result = 
                            m_chainman.ProcessTransaction(ptx);
const TxValidationState& state = result.m_state;

if (result.m_result_type == MempoolAcceptResult::ResultType::VALID) {
    ProcessValidTx(
      pfrom.GetId(), 
      ptx, 
      result.m_replaced_transactions
    );
    pfrom.m_last_tx_time = GetTime<std::chrono::seconds>();
}
if (state.IsInvalid()) {
    if (auto package_to_validate{
      ProcessInvalidTx(
        pfrom.GetId(), 
        ptx, 
        state, 
        /*first_time_failure=*/true)
      }) {
        const auto package_result{
          ProcessNewPackage(
            m_chainman.ActiveChainstate(),
            m_mempool,
            package_to_validate->m_txns,
            /*test_accept=*/false,
            /*client_maxfeerate=*/std::nullopt
            )
          };
        ProcessPackageResult(
          package_to_validate.value(), 
          package_result
        );
    }
}

return;
```

Two outcomes:

- **Valid**: the transaction is already in the mempool at this point. `ProcessValidTx` handles the post-acceptance bookkeeping and queues it for broadcast and we then update `pfrom.m_last_tx_time`.
- **Invalid**: `ProcessInvalidTx` records the failure and decides whether there's anything more to try:
  - `TX_RECONSIDERABLE` (e.g. fee too low): the `wtxid` is inserted into `RecentRejectsReconsiderableFilter` so we won't try to submit it alone again. On first-time failure `Find1P1CPackage` looks for a child in the orphanage that already spends it from the same peer. If found, we attempt CPFP (Child-Pays-For-Parent) acceptance by running [`ProcessNewPackage`](https://github.com/bitcoin/bitcoin/blob/v31.0/src/validation.cpp#L1804) and `ProcessPackageResult`.

  - `TX_MISSING_INPUTS`: we don't yet have the parent transaction(s) this one depends on. The transaction is treated as a candidate orphan and, if accepted, added to the orphanage with a set of candidate peers (the relayer plus any other peer that announced this same hash) for parent fetching.

  - For most other invalid results, the `wtxid` is inserted into `RecentRejectsFilter` so we won't redownload it. Two edge cases:
    <ol type="a">
      <li>If the failure is <code>TX_INPUTS_NOT_STANDARD</code> and the transaction has a witness, the <code>txid</code> is added to the filter as well, because that failure is witness-independent;</li>
      <li>If the failure is <code>TX_WITNESS_STRIPPED</code>, nothing is inserted into the reject filter because the witness was stripped in transit, so a different peer might still have the complete version.</li>
    </ol>

## Why we keep transactions we rejected

Both `ProcessValidTx` and `ProcessInvalidTx` can feed a transaction into `AddToCompactExtraTransactions`, which maintains a small ring buffer of transactions the node didn't necessarily accept, but might need later.

The reason is compact block reconstruction. When a peer sends us a compact block (a bandwidth-efficient block format that sends short transaction IDs rather than full transactions), we need to match those IDs against transactions we've seen. If a referenced transaction isn't in our mempool, having it in this buffer saves a round trip.

The two sources:

<div class="with-aside">

- `ProcessValidTx` contributes the RBF-replaced transactions. They are out of our mempool, but a peer's next compact block could still could still reference them if a miner included the original tx instead of the replacement.
- `ProcessInvalidTx` contributes any transactions that meet the following set of conditions: first-time failure, witness wasn't stripped, not already orphaned, and under 100 kB of dynamic memory usage.

<aside class="side">
  RBF means Replace-By-Fee, where a transaction is replaced by a higher-fee version
</aside>

</div>

It's a small cost, a bounded ring buffer, for a meaningful benefit: fewer network round trips during block propagation.

## Wrapping up

What looks like a one-liner from the outside ("peer sent a tx, put it in the mempool") is actually a layered decision tree. Before a single signature is checked, the node has already asked: should I be listening to this peer at all? Am I caught up enough to judge? Is this my own private broadcast coming back to me? Have I already seen, rejected, or orphaned this hash? Do I have a child waiting on it?

Each of these checks exists for a concrete reason: bandwidth, privacy, DoS resistance, or to make room for fee-bumping via CPFP, which Bitcoin Core currently handles through 1P1C package acceptance. The actual mempool acceptance, the part that verifies scripts, checks policy and updates chainstate, is a single call ([`ProcessTransaction`](https://github.com/bitcoin/bitcoin/blob/v31.0/src/validation.cpp#L4480) or [`ProcessNewPackage`](https://github.com/bitcoin/bitcoin/blob/v31.0/src/validation.cpp#L1804)) sitting in the middle of all this routing logic. That validation pipeline is its own story.

What I find interesting about reading this code is how much of it is not validation at all. It is the node deciding what is worth its time, what to remember, what to forget, and who to trust enough to bother asking. The mempool is downstream of all that.