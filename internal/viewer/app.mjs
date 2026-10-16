import { responseState, retryDelay, waitForICE } from './state.mjs';

const video = document.querySelector('#video');
const sound = document.querySelector('#sound');
const hint = document.querySelector('#hint');
const waiting = document.querySelector('#waiting');
const status = document.querySelector('#status');
const labels = {
  connecting: ['CONNECTING', 'Finding the signal.', 'Connecting to the viewing room…'],
  offline: ['OFFLINE', 'Quiet for now.', 'The host is not sharing. This room reconnects automatically.'],
  full: ['ROOM FULL', 'All seats taken.', 'This room is at capacity. We’ll try again shortly.'],
  live: ['LIVE', 'You’re in.', 'The shared screen is live.'],
};
let state = 'connecting';
let session = null;
let generation = 0;
let timer;
let stopped = true;
let audioWanted = false;

function show(next) {
  state = next;
  status.dataset.state = next;
  document.querySelector('#status-text').textContent = labels[next][0];
  document.querySelector('#waiting-title').textContent = labels[next][1];
  document.querySelector('#waiting-detail').textContent = labels[next][2];
  waiting.hidden = next === 'live';
}

function closeSession() {
  const old = session;
  session = null;
  if (old) {
    old.controller.abort();
    clearTimeout(old.disconnectTimer);
    old.pc.close();
  }
  video.srcObject = null;
  sound.disabled = true;
}

function current(s) { return !stopped && session === s && s.generation === generation; }

function failed(s) {
  if (!current(s)) return;
  closeSession();
  show('connecting');
  // The single polling loop owns retries, including connection failures.
}

async function request(url, options, signal) {
  const controller = new AbortController();
  const abort = () => controller.abort();
  signal.addEventListener('abort', abort, { once: true });
  if (signal.aborted) controller.abort();
  const timeout = setTimeout(abort, 12000);
  try {
    const response = await fetch(url, { ...options, signal: controller.signal, cache: 'no-store' });
    if (!response.ok) throw Object.assign(new Error('Signaling unavailable'), { status: response.status });
    return await response.json();
  } finally {
    clearTimeout(timeout);
    signal.removeEventListener('abort', abort);
  }
}

async function play(s) {
  try {
    await video.play();
    if (!current(s)) return;
    sound.textContent = video.muted ? 'Enable sound' : 'Mute sound';
    hint.textContent = video.muted ? 'Playback is muted. Sound is your call.' : 'You’re watching with sound.';
  } catch {
    if (!current(s)) return;
    sound.textContent = audioWanted ? 'Play with sound' : 'Play stream';
    hint.textContent = 'Playback paused by your browser. Press Play to continue.';
  }
}

async function connect(epoch) {
  const pc = new RTCPeerConnection();
  const s = { pc, controller: new AbortController(), generation: epoch, disconnectTimer: null };
  session = s;
  show('connecting');
  const stream = new MediaStream();
  video.srcObject = stream;
  video.muted = !audioWanted;
  pc.addEventListener('track', (event) => {
    if (!current(s)) return;
    stream.addTrack(event.track);
    event.track.addEventListener('ended', () => failed(s), { once: true });
    sound.disabled = false;
    void play(s);
  });
  pc.addEventListener('connectionstatechange', () => {
    if (!current(s)) return;
    clearTimeout(s.disconnectTimer);
    if (pc.connectionState === 'connected') show('live');
    if (pc.connectionState === 'failed' || pc.connectionState === 'closed') failed(s);
    if (pc.connectionState === 'disconnected') {
      show('connecting');
      s.disconnectTimer = setTimeout(() => failed(s), 5000);
    }
  });
  try {
    pc.addTransceiver('video', { direction: 'recvonly' });
    pc.addTransceiver('audio', { direction: 'recvonly' });
    await pc.setLocalDescription(await pc.createOffer());
    await waitForICE(pc, s.controller.signal);
    if (!current(s)) return;
    const answer = await request('/api/watch', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ type: pc.localDescription.type, sdp: pc.localDescription.sdp }),
    }, s.controller.signal);
    if (!current(s)) return;
    if (answer.type !== 'answer' || typeof answer.sdp !== 'string') throw new Error('Invalid answer');
    await pc.setRemoteDescription(answer);
    if (!current(s)) return;
    // A successful HTTP exchange does not guarantee a working media route.
    s.disconnectTimer = setTimeout(() => {
      if (pc.connectionState !== 'connected') failed(s);
    }, 15000);
  } catch (error) {
    if (!current(s)) return;
    closeSession();
    show(responseState(error.status));
  }
}

let pollController;
async function poll(epoch) {
  if (stopped || epoch !== generation) return;
  pollController = new AbortController();
  try {
    const result = await request('/api/status', {}, pollController.signal);
    if (stopped || epoch !== generation) return;
    if (typeof result.live !== 'boolean') throw new Error('Invalid status');
    if (!result.live) {
      closeSession();
      show('offline');
    } else if (!session) {
      await connect(epoch);
    }
  } catch (error) {
    if (stopped || epoch !== generation) return;
    // A transient status failure must not interrupt an established stream.
    if (!session) show(responseState(error.status));
  } finally {
    if (!stopped && epoch === generation) timer = setTimeout(() => void poll(epoch), retryDelay(state));
  }
}

sound.addEventListener('click', () => {
  if (!session) return;
  if (!video.paused && !video.muted) {
    audioWanted = false;
    video.muted = true;
  } else {
    audioWanted = true;
    video.muted = false;
  }
  void play(session);
});
video.addEventListener('volumechange', () => {
  audioWanted = !video.muted;
  sound.textContent = video.muted ? 'Enable sound' : 'Mute sound';
});
document.querySelector('#fullscreen').addEventListener('click', async () => {
  try {
    if (document.fullscreenElement) await document.exitFullscreen();
    else if (document.querySelector('#screen').requestFullscreen) await document.querySelector('#screen').requestFullscreen();
    else if (video.webkitEnterFullscreen) video.webkitEnterFullscreen();
    else hint.textContent = 'Use your browser’s video controls for fullscreen.';
  } catch { hint.textContent = 'Fullscreen unavailable. Try the video controls.'; }
});

function start() {
  if (!stopped) return;
  if (!window.RTCPeerConnection) {
    show('offline');
    document.querySelector('#waiting-detail').textContent = 'This browser does not support WebRTC. Try a current browser.';
    return;
  }
  stopped = false;
  void poll(++generation);
}
window.addEventListener('pagehide', () => {
  stopped = true;
  generation++;
  clearTimeout(timer);
  pollController?.abort();
  closeSession();
});
window.addEventListener('pageshow', start);
start();
