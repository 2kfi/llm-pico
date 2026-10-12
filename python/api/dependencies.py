from __future__ import annotations

import hmac
from fastapi import HTTPException, Request

from core.auth import extract_bearer, verify_api_key, check_ip_allowed, verify_hmac_signature


async def require_api_key(request: Request) -> dict:
    """FastAPI dependency: verify a valid Bearer API key (user or master).

    Returns the user_key dict. Raises 401 on failure.
    """
    auth_header = request.headers.get("Authorization")
    raw_key = extract_bearer(auth_header)
    if not raw_key:
        raise HTTPException(status_code=401, detail={
            "error": {"message": "Missing or invalid Authorization header",
                       "type": "unauthorized", "code": 401}
        })
    config = request.app.state.config
    master_key = config.general_settings.master_key
    user_key = await verify_api_key(raw_key, master_key)
    if user_key is None:
        raise HTTPException(status_code=401, detail={
            "error": {"message": "Invalid API key",
                       "type": "unauthorized", "code": 401}
        })

    # IP allowlist check (non-admin keys only)
    if user_key.get("role") == "user":
        client_ip = request.client.host if request.client else ""
        ip_list = user_key.get("ip_allowlist")
        if ip_list and not check_ip_allowed(user_key["key_hash"], client_ip, ip_list):
            raise HTTPException(status_code=403, detail={
                "error": {"message": "IP not allowed",
                           "type": "forbidden", "code": 403}
            })

    # HMAC request signing check
    if config.general_settings.hmac_enabled:
        signature = request.headers.get("X-Signature", "")
        if not signature:
            raise HTTPException(status_code=401, detail={
                "error": {"message": "Missing X-Signature header",
                           "type": "unauthorized", "code": 401}
            })
        body = await request.body()
        hmac_secret = config.general_settings.hmac_secret
        if not verify_hmac_signature(hmac_secret, body, signature):
            raise HTTPException(status_code=401, detail={
                "error": {"message": "Invalid HMAC signature",
                           "type": "unauthorized", "code": 401}
            })

    return user_key


async def require_master_key(request: Request) -> str:
    """FastAPI dependency: require the master API key (admin only).

    Accepts either:
    - Raw master key (legacy): Bearer <raw-key>
    - Key hash (new frontend): Bearer <sha256-hash-of-raw-key>

    The stored master_key may be a raw key (old init) or a hash (new init).
    We compare the bearer token against both the stored value and its hash.
    """
    config = request.app.state.config
    master_key = config.general_settings.master_key
    auth_header = request.headers.get("Authorization")
    raw_key = extract_bearer(auth_header)
    if not raw_key:
        raise HTTPException(status_code=401, detail={
            "error": {"message": "Invalid or missing master API key",
                       "type": "unauthorized", "code": 401}
        })

    # Direct match (raw key == stored value)
    if hmac.compare_digest(raw_key, master_key):
        return raw_key

    # Hash match: stored value is hash, bearer is hash of the same raw key
    # Or: stored value is raw key, bearer is hash of that raw key
    from core.auth import hash_key
    if hmac.compare_digest(raw_key, hash_key(master_key)):
        return raw_key

    raise HTTPException(status_code=401, detail={
        "error": {"message": "Invalid or missing master API key",
                   "type": "unauthorized", "code": 401}
    })
