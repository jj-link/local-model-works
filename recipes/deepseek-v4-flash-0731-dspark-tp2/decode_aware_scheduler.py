"""vLLM scheduler policy that bounds prefill interference with active decode."""

from __future__ import annotations

import os

from vllm.logger import init_logger
from vllm.v1.core.sched.async_scheduler import AsyncScheduler

logger = init_logger(__name__)


class DecodeAwareScheduler(AsyncScheduler):
    """Use efficient prefill chunks on a cadence around active decode.

    The default scheduler lets an in-progress prefill consume a large mixed
    forward pass on every step. While decode is active, this scheduler permits
    the configured long-prefill chunk once per cadence interval and temporarily
    caps all other scheduling passes to the DSpark verify width. This preserves
    async scheduling and does not mutate request lifecycle state.
    """

    def __init__(self, *args, **kwargs) -> None:
        super().__init__(*args, **kwargs)
        self.decode_aware_prefill_interval = int(
            os.environ.get("DECODE_AWARE_PREFILL_INTERVAL", "1")
        )
        self.decode_aware_throttled_tokens = int(
            os.environ.get("DECODE_AWARE_THROTTLED_TOKENS", "1")
        )
        if self.decode_aware_prefill_interval < 1:
            raise ValueError("DECODE_AWARE_PREFILL_INTERVAL must be at least 1")
        if self.decode_aware_throttled_tokens < 1:
            raise ValueError("DECODE_AWARE_THROTTLED_TOKENS must be at least 1")
        self._logged_cadence_cap = False
        logger.info(
            "Decode-aware prefill cadence: interval=%d throttled_tokens=%d",
            self.decode_aware_prefill_interval,
            self.decode_aware_throttled_tokens,
        )

    def schedule(self, throttle_prefills: bool = False):
        has_active_decode = any(
            request.num_computed_tokens >= request.num_prompt_tokens
            for request in self.running
        )
        next_step = self.current_step + 1
        cadence_cap = (
            has_active_decode
            and self.decode_aware_prefill_interval > 1
            and next_step % self.decode_aware_prefill_interval != 0
        )
        configured_threshold = self.scheduler_config.long_prefill_token_threshold
        if cadence_cap:
            effective_threshold = self.decode_aware_throttled_tokens
            if configured_threshold > 0:
                effective_threshold = min(
                    configured_threshold, effective_threshold
                )
            self.scheduler_config.long_prefill_token_threshold = effective_threshold
            if not self._logged_cadence_cap:
                logger.warning(
                    "Decode-aware cadence cap active: interval=%d tokens=%d",
                    self.decode_aware_prefill_interval,
                    effective_threshold,
                )
                self._logged_cadence_cap = True
        try:
            return super().schedule(throttle_prefills=throttle_prefills)
        finally:
            self.scheduler_config.long_prefill_token_threshold = configured_threshold
