export function responseState(status) {
  if (status === 503) return 'offline';
  if (status === 429) return 'full';
  return 'connecting';
}

export function retryDelay(state, random = Math.random()) {
  return (state === 'full' ? 15000 : 5000) + Math.floor(random * 1000);
}

// Non-trickle signaling must include the completed local ICE description.
export function waitForICE(pc, signal, timeout = 10000) {
  return new Promise((resolve, reject) => {
    let timer;
    const finish = (error) => {
      clearTimeout(timer);
      pc.removeEventListener('icegatheringstatechange', change);
      signal.removeEventListener('abort', abort);
      if (error) reject(error); else resolve();
    };
    const change = () => { if (pc.iceGatheringState === 'complete') finish(); };
    const abort = () => finish(new DOMException('Session cancelled', 'AbortError'));
    if (signal.aborted) { abort(); return; }
    if (pc.iceGatheringState === 'complete') { resolve(); return; }
    pc.addEventListener('icegatheringstatechange', change);
    signal.addEventListener('abort', abort, { once: true });
    timer = setTimeout(() => finish(new Error('ICE gathering timed out')), timeout);
  });
}
