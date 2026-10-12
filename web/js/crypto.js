window.llmPico = window.llmPico || {};

(function(ns) {
  const subtle = crypto.subtle;

  async function hashKey(key) {
    const data = new TextEncoder().encode(key);
    const hash = await subtle.digest('SHA-256', data);
    return [...new Uint8Array(hash)].map(b => b.toString(16).padStart(2, '0')).join('');
  }

  async function hmacSign(key, body) {
    const enc = new TextEncoder();
    const k = await subtle.importKey('raw', enc.encode(key), { name: 'HMAC', hash: 'SHA-256' }, false, ['sign']);
    const sig = await subtle.sign('HMAC', k, enc.encode(body));
    return [...new Uint8Array(sig)].map(b => b.toString(16).padStart(2, '0')).join('');
  }

  function storeSession(keyHash, token) {
    sessionStorage.setItem('lp_keyHash', keyHash);
    sessionStorage.setItem('lp_token', token);
  }

  function getSession() {
    const keyHash = sessionStorage.getItem('lp_keyHash');
    const token = sessionStorage.getItem('lp_token');
    return keyHash && token ? { keyHash, token } : null;
  }

  function clearSession() {
    sessionStorage.removeItem('lp_keyHash');
    sessionStorage.removeItem('lp_token');
  }

  function isLoggedIn() {
    return !!getSession();
  }

  Object.assign(ns, { hashKey, hmacSign, storeSession, getSession, clearSession, isLoggedIn });
})(window.llmPico);
