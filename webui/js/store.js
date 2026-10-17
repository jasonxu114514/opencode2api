// Shared console state with a minimal change subscription so page modules
// re-render independently.
const state = {
  config: null,
  monitor: null,
  debug: null,
  lastPlayground: null,
};

const listeners = new Map();

export function get(key) {
  return state[key];
}

export function set(key, value) {
  state[key] = value;
  const subscribers = listeners.get(key);
  if (!subscribers) return;
  for (const handler of [...subscribers]) handler(value);
}

export function on(key, handler) {
  let subscribers = listeners.get(key);
  if (!subscribers) listeners.set(key, (subscribers = new Set()));
  subscribers.add(handler);
  return () => subscribers.delete(handler);
}
