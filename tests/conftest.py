from __future__ import annotations

import pytest

from core.config import Config, GeneralSettings, LitellmParams, ModelEntry, RouterSettings


@pytest.fixture
def config(tmp_path):
    return Config(
        general_settings=GeneralSettings(master_key="sk-pico-master-test"),
        router_settings=RouterSettings(
            routing_strategy="simple-shuffle",
            num_retries=2,
            cooldown_time=45,
            allowed_fails=1,
        ),
    )


@pytest.fixture
def single_model_config(config):
    config.model_list = [
        ModelEntry(
            model_name="test-model",
            litellm_params=LitellmParams(
                model="openai/gpt-4",
                api_key="sk-test-key-1",
                api_base="https://api.openai.com/v1",
            ),
        ),
    ]
    return config


@pytest.fixture
def multi_key_config(config):
    config.model_list = [
        ModelEntry(
            model_name="test-model",
            litellm_params=LitellmParams(
                model="openai/gpt-4",
                api_key="sk-test-key-1",
                api_base="https://api.openai.com/v1",
            ),
        ),
        ModelEntry(
            model_name="test-model",
            litellm_params=LitellmParams(
                model="openai/gpt-4",
                api_key="sk-test-key-2",
                api_base="https://api.openai.com/v1",
            ),
        ),
    ]
    return config


@pytest.fixture
def dual_group_config(config):
    config.model_list = [
        ModelEntry(
            model_name="test-model",
            litellm_params=LitellmParams(
                model="openai/gpt-4",
                api_key="sk-oa-key",
                api_base="https://api.openai.com/v1",
            ),
        ),
        ModelEntry(
            model_name="test-model",
            litellm_params=LitellmParams(
                model="groq/llama3",
                api_key="gsk-gr-key",
                api_base="https://api.groq.com/openai/v1",
            ),
        ),
    ]
    return config
