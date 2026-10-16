// Run directly: node --test internal/viewer/state.test.mjs
import test from 'node:test';
import assert from 'node:assert/strict';
import { responseState, retryDelay, waitForICE } from './state.mjs';

test('HTTP failures distinguish offline, capacity, and transient errors', () => {
  assert.equal(responseState(503), 'offline');
  assert.equal(responseState(429), 'full');
  for (const status of [undefined, 400, 500, 502]) assert.equal(responseState(status), 'connecting');
});

test('retries have a floor, jitter, and longer capacity backoff', () => {
  assert.equal(retryDelay('full', 0), 15000);
  assert.equal(retryDelay('full', .999), 15999);
  for (const state of ['offline', 'connecting', 'live']) {
    assert.equal(retryDelay(state, 0), 5000);
    assert.equal(retryDelay(state, .999), 5999);
  }
});

class Peer extends EventTarget {
  iceGatheringState = 'gathering';
  complete() {
    this.iceGatheringState = 'complete';
    this.dispatchEvent(new Event('icegatheringstatechange'));
  }
}

test('ICE waits for complete gathering, not an intermediate event', async () => {
  const pc = new Peer();
  const controller = new AbortController();
  let complete = false;
  const pending = waitForICE(pc, controller.signal, 100).then(() => { complete = true; });
  pc.dispatchEvent(new Event('icegatheringstatechange'));
  await Promise.resolve();
  assert.equal(complete, false);
  pc.complete();
  await pending;
  assert.equal(complete, true);
  controller.abort(); // Listener must already be removed.
});

test('already-complete ICE resolves immediately', async () => {
  const pc = new Peer();
  pc.complete();
  await waitForICE(pc, new AbortController().signal, 10);
});

test('ICE supports cancellation before and during gathering', async () => {
  for (const alreadyAborted of [true, false]) {
    const controller = new AbortController();
    if (alreadyAborted) controller.abort();
    const pending = waitForICE(new Peer(), controller.signal, 100);
    if (!alreadyAborted) controller.abort();
    await assert.rejects(pending, { name: 'AbortError' });
  }
});

test('ICE timeout rejects instead of leaving a session hanging', async () => {
  await assert.rejects(waitForICE(new Peer(), new AbortController().signal, 5), /timed out/);
});
