window.llmPico = window.llmPico || {};

(function(ns) {
  const PROVIDERS = [
    { id: 'openai', name: 'OpenAI', category: 'cloud', icon: '<svg viewBox="0 0 18 18" fill="currentColor"><path d="M9 1.5a7.5 7.5 0 100 15 7.5 7.5 0 000-15zM6.3 12.1c-.1.5-.5.8-1 .6-.5-.2-.7-.7-.5-1.2.1-.4.5-.7.9-.6.5.1.7.6.6 1.1zm1.2-2.8c-.1.6-.6 1-1.2.8-.5-.2-.7-.8-.5-1.3.1-.5.6-.9 1.1-.7.6.2.8.7.6 1.2zM12.3 9c-.2.7-.8 1.1-1.5.9-.6-.2-.9-.8-.7-1.4.2-.6.8-1 1.4-.8.7.2.9.8.8 1.3zm.3-1.8c-.7.3-1.5-.1-1.8-.7-.3-.7 0-1.5.6-1.8.7-.3 1.5.1 1.8.7.3.7 0 1.5-.6 1.8z"/></svg>', baseUrl: 'https://api.openai.com/v1', auth: 'bearer', fields: ['api_key'], capabilities: ['Text', 'Vision', 'Image', 'Embeddings'] },
    { id: 'anthropic', name: 'Anthropic', category: 'cloud', icon: '<svg viewBox="0 0 18 18" fill="currentColor"><path d="M9 2L3 16h3l1.5-4h3L12 16h3L9 2zm0 4.5l1.8 5.5H7.2L9 6.5z"/></svg>', baseUrl: 'https://api.anthropic.com', auth: 'x-api-key', fields: ['api_key'], modelsEndpoint: null, capabilities: ['Text', 'Vision'] },
    { id: 'google', name: 'Google AI Studio', category: 'cloud', icon: '<svg viewBox="0 0 18 18" fill="currentColor"><path d="M9 3.5c1.4 0 2.6.5 3.5 1.4l-1.4 1.4c-.6-.6-1.4-.9-2.1-.9-1.8 0-3.2 1.5-3.2 3.2s1.5 3.2 3.2 3.2c1.5 0 2.7-.9 3-2.2H9v-1.9h5.2c.1.5.1 1 .1 1.5 0 3.3-2.2 5.5-5.3 5.5-3 0-5.5-2.5-5.5-5.5S6 3.5 9 3.5z"/></svg>', baseUrl: 'https://generativelanguage.googleapis.com/v1beta', auth: 'query_key', fields: ['api_key'], capabilities: ['Text', 'Vision', 'Audio'] },
    { id: 'openrouter', name: 'OpenRouter', category: 'aggregator', icon: '<svg viewBox="0 0 18 18" fill="none" stroke="currentColor" stroke-width="1.5"><circle cx="9" cy="9" r="6"/><path d="M6 9h6M9 6v6"/></svg>', baseUrl: 'https://openrouter.ai/api/v1', auth: 'bearer', fields: ['api_key'], capabilities: ['Text', 'Vision'] },
    { id: 'cloudflare', name: 'Cloudflare Workers AI', category: 'cloud', icon: '<svg viewBox="0 0 18 18" fill="currentColor"><path d="M12.5 7.5c0-2-1.6-3.5-3.5-3.5-1.7 0-3.1 1.2-3.4 2.8-.1 0-.3.1-.4.1-1.1 0-2 .9-2 2s.9 2 2 2h5.8c.8 0 1.5-.7 1.5-1.5 0-.8-.6-1.4-1.4-1.4-.1 0-.3 0-.6.5z"/></svg>', baseUrl: 'https://api.cloudflare.com/client/v4', auth: 'bearer', fields: ['api_key', 'account_id'], capabilities: ['Text', 'Image'] },
    { id: 'groq', name: 'Groq', category: 'cloud', icon: '<svg viewBox="0 0 18 18" fill="currentColor"><path d="M4 5h10v1.5H4V5zm0 3h7v1.5H4V8zm0 3h10v1.5H4V11z"/></svg>', baseUrl: 'https://api.groq.com/openai/v1', auth: 'bearer', fields: ['api_key'], capabilities: ['Text'] },
    { id: 'zhipu', name: 'ZHIPU AI', category: 'cloud', icon: '<svg viewBox="0 0 18 18" fill="currentColor"><path d="M9 3l-5 8h10L9 3zm0 10a1.5 1.5 0 110 3 1.5 1.5 0 010-3z"/></svg>', baseUrl: 'https://open.bigmodel.cn/api/paas/v4', auth: 'bearer', fields: ['api_key'], capabilities: ['Text', 'Vision'] },
    { id: 'nvidia', name: 'NVIDIA NIM', category: 'cloud', icon: '<svg viewBox="0 0 18 18" fill="currentColor"><path d="M3 5h3v8H3V5zm4 2h3v6H7V7zm4-1h3v7h-3V6z"/></svg>', baseUrl: 'https://integrate.api.nvidia.com/v1', auth: 'bearer', fields: ['api_key'], capabilities: ['Text'] },
    { id: 'requesty', name: 'Requesty AI', category: 'aggregator', icon: '<svg viewBox="0 0 18 18" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M3 9h12M9 3v12"/></svg>', baseUrl: 'https://api.requesty.ai/v1', auth: 'bearer', fields: ['api_key'], capabilities: ['Text'] },
    { id: 'opencode-zen', name: 'OpenCode ZEN', category: 'aggregator', icon: '<svg viewBox="0 0 18 18" fill="none" stroke="currentColor" stroke-width="1.5"><path d="M5 6l4 3-4 3M10 6l4 3-4 3"/></svg>', baseUrl: 'https://api.opencode.ai/v1', auth: 'bearer', fields: ['api_key'], capabilities: ['Text'] },
    { id: 'ollama-cloud', name: 'Ollama Cloud', category: 'local', icon: '<svg viewBox="0 0 18 18" fill="currentColor"><path d="M9 2C5.1 2 2 5.1 2 9s3.1 7 7 7 7-3.1 7-7-3.1-7-7-7zm0 2c2.8 0 5 2.2 5 5s-2.2 5-5 5-5-2.2-5-5 2.2-5 5-5zm-1 3v4l3.5 2.1.5-.9-3-1.8V7H8z"/></svg>', baseUrl: 'https://ollama.com/v1', auth: 'bearer', fields: ['api_key'], capabilities: ['Text'] },
    { id: 'together', name: 'Together AI', category: 'cloud', icon: '<svg viewBox="0 0 18 18" fill="currentColor"><path d="M5 4l4 5-4 5M10 4l4 5-4 5"/></svg>', baseUrl: 'https://api.together.xyz/v1', auth: 'bearer', fields: ['api_key'], capabilities: ['Text', 'Image'] },
    { id: 'mistral', name: 'Mistral', category: 'cloud', icon: '<svg viewBox="0 0 18 18" fill="currentColor"><path d="M4 5h10v2H4V5zm0 3h7v2H4V8zm0 3h10v2H4v-2z"/></svg>', baseUrl: 'https://api.mistral.ai/v1', auth: 'bearer', fields: ['api_key'], capabilities: ['Text', 'Vision'] },
    { id: 'deepseek', name: 'Deepseek', category: 'cloud', icon: '<svg viewBox="0 0 18 18" fill="currentColor"><path d="M9 3c-3.3 0-6 2.7-6 6s2.7 6 6 6 6-2.7 6-6-2.7-6-6-6zm0 2c1.1 0 2 .9 2 2s-.9 2-2 2-2-.9-2-2 .9-2 2-2zm0 8c-2.2 0-4-1.8-4-4h2c0 1.1.9 2 2 2s2-.9 2-2h2c0 2.2-1.8 4-4 4z"/></svg>', baseUrl: 'https://api.deepseek.com/v1', auth: 'bearer', fields: ['api_key'], capabilities: ['Text'] },
    { id: 'perplexity', name: 'Perplexity', category: 'cloud', icon: '<svg viewBox="0 0 18 18" fill="none" stroke="currentColor" stroke-width="1.5"><circle cx="9" cy="9" r="5"/><path d="M9 4v10M4 9h10"/></svg>', baseUrl: 'https://api.perplexity.ai', auth: 'bearer', fields: ['api_key'], capabilities: ['Text'] },
    { id: 'openai-compat', name: 'OpenAI-Compatible API', category: 'custom', icon: '<svg viewBox="0 0 18 18" fill="none" stroke="currentColor" stroke-width="1.5"><rect x="3" y="3" width="12" height="12" rx="2"/><path d="M7 7h4M7 10h2"/></svg>', baseUrl: null, auth: 'bearer', fields: ['name', 'base_url', 'api_key'], custom: true, capabilities: ['Text'] },
    { id: 'anthropic-compat', name: 'Anthropic-Compatible API', category: 'custom', icon: '<svg viewBox="0 0 18 18" fill="none" stroke="currentColor" stroke-width="1.5"><rect x="3" y="3" width="12" height="12" rx="2"/><path d="M7 7h4M7 10h2"/></svg>', baseUrl: null, auth: 'x-api-key', fields: ['name', 'base_url', 'api_key'], custom: true, capabilities: ['Text'] },
  ];

  const CAPABILITIES = ['Text', 'Image', 'Vision', 'STT', 'TTS', 'Embeddings'];

  function detectCapabilities(modelId) {
    const id = modelId.toLowerCase();
    const caps = ['Text'];
    if (/whisper|stt|speech.to.text/.test(id)) caps.push('STT');
    if (/tts|speech|nova.sonic/.test(id)) caps.push('TTS');
    if (/embed/.test(id)) caps.push('Embeddings');
    if (/vision|vl|gpt.4o|claude.3|multi.modal/.test(id)) caps.push('Vision', 'Image');
    if (/\.(jpg|png|webp)|image.gen|dall.e|flux|stable.diffusion/.test(id)) caps.push('Image');
    return caps;
  }

  function getProviderById(id) {
    return PROVIDERS.find(p => p.id === id) || null;
  }

  function getAuthHeaders(provider, apiKey) {
    if (provider.auth === 'bearer') return { 'Authorization': 'Bearer ' + apiKey };
    if (provider.auth === 'x-api-key') return { 'x-api-key': apiKey };
    return {};
  }

  Object.assign(ns, { PROVIDERS, CAPABILITIES, detectCapabilities, getProviderById, getAuthHeaders });
})(window.llmPico);
