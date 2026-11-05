FROM ghcr.io/miaai-lab/glm-5.3-flash-exl3-2x-dgx-sparks-tensorfold@sha256:b47c19d66633f27cbe37da13fbc580363f466c08b9529feab1eecb1a4b904bf1

# Native HTTP correction only; keep the upstream kernels, weights and notices.
COPY internal/recipe/repositorycompiler/testdata/glm53-flash-exl3-tensorfold-spark-tp2/LICENSE internal/recipe/repositorycompiler/testdata/glm53-flash-exl3-tensorfold-spark-tp2/NOTICE /opt/local-model-works/
COPY recipes/glm53-flash-exl3-tensorfold-spark-tp2/model-context.patch /opt/local-model-works/model-context.patch
RUN patch --batch --fuzz=0 --directory=/usr/local/lib/python3.12/dist-packages --strip=0 --input=/opt/local-model-works/model-context.patch

# This is not the unmodified upstream patch stack. Use the explicit image and
# PREPARE=0 only after provisioning it and the pinned snapshots on both ranks.
LABEL tf.patches="lmw-model-context-c4cab25d2d36"
