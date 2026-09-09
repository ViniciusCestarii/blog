---
title: Teaching an Agent to Hunt Live Mutants in Bitcoin Core
date: 2026-09-09
---

I got interested in mutation testing while helping to add test coverage to [Bitcoin Core](https://github.com/bitcoin/bitcoin/), and tests are like grandma's desserts: always welcome.

Mutation testing is a test for the tests. It injects small deliberate changes into the source, called mutants, and runs the test suite against each one. A mutant the test suite catches is killed. One that survives is a live mutant, and a live mutant is a gap: the code could have been wrong in that exact way and nothing would have flagged it.

That matters more in consensus code than anywhere else. Coverage says a line executed, not that anything asserted on what it produced, and a consensus line that runs unchecked is not a failing test. It is a fork risk that ships green.

So I built [`mutant-harness`](https://github.com/ViniciusCestarii/mutant-harness). It runs an agent inside a throwaway Docker container holding a clone of Bitcoin Core and a clone of [`bitcoin/bips`](https://github.com/bitcoin/bips), and the agent's job is to mutate to find live mutants.

## How it works

You point it at one file:

```sh
mutant-harness --file src/script/interpreter.cpp
```

From there the agent works in a fixed order. It reads the whole file, greps the BIPs for the constants and function names in it to find the rules the file is the implementation of, then greps `src/test/` and `test/functional/` to find out which of those rules nobody checks. The thin spots are where the mutants go.

Each one comes out as a one-change patch plus a manifest entry: what it breaks, a concrete input that triggers it, the BIP rule it violates, and the agent's own best argument that the mutant is an equivalent mutant.

<div class="with-aside">

The value of that last field is not in the argument the agent writes, it is in making it think about the question. To claim its own mutant changes nothing, it has to trace what the mutated line actually feeds, and the mutants come out sharper for it.

<aside class="side">
  <p>An equivalent mutant is a code change that preserves the same program's behavior, so no test can ever kill it.</p>
</aside>

</div>

A separate script, `mutant-verify`, does the expensive part: apply, build, `ctest`, functional suite, one mutant at a time, reverting in between. Survive all of it and the script reports the mutant as `live`.

## Why not just use bcore-mutation?

[`bcore-mutation`](https://github.com/brunoerg/bcore-mutation) by brunoerg already does this the classical way: a fixed set of operators applied mechanically across the source, then the suite runs against each mutant. It is fast, deterministic, and cheap.

`mutant-harness` is none of those things. It is slow, expensive, and gives a different answer every run.

What it buys in exchange is semantics. A classical operator flips `<` to `<=` because that is what the rule says, with no idea what the comparison guards. An agent that has read the surrounding code and the relevant BIP can reorder statements, drop an entire block, or reproduce the kind of mistake a tired human would actually make. Those mutants live in the space between "syntactically valid" and "plausibly wrong", which is hard to reach by rule alone.

The two are not competing but complementary. Run the fast tool for breadth, and reach for the slow one where correctness depends on what the code means, not just on the shape of an expression.

## Does it work?

Yes. Two PRs came out of it, each adding tests for mutants the existing suite ran straight past.

[bitcoin/bitcoin#36128](https://github.com/bitcoin/bitcoin/pull/36128), in `interpreter.cpp`:

- P2WSH compared the witness script hash with less bytes than `32`. Some bytes of the hash went unchecked and nothing failed.
- The `MAX_OPS_PER_SCRIPT` guard dropped `WITNESS_V0`, so the opcode limit stopped applying to segwit v0 scripts.
- Tapscript minimal-`IF` accepted `vch[0] > 1` instead of `!= 1`, silently allowing a non-minimal condition byte.

[bitcoin/bitcoin#36130](https://github.com/bitcoin/bitcoin/pull/36130), in `tx_verify.cpp`:

- `IsFinalTx` turned "every input is final" into "any input is final". One `SEQUENCE_FINAL` input was enough to make a non-final transaction look final.
- `CalculateSequenceLocks` narrowed BIP68 enforcement from `version >= 2` to `version == 2`, exempting every later version.
- `GetLegacySigOpCount` counted `scriptSig` sigops in accurate mode instead of legacy mode.

More PRs like these are coming, and more mutants will die!