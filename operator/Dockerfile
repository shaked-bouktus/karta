# SPDX-License-Identifier: Apache-2.0
# Copyright (c) 2026 NVIDIA Corporation

FROM gcr.io/distroless/static-debian13:nonroot
ARG TARGETPLATFORM
COPY --chmod=0755 ${TARGETPLATFORM}/karta-operator /karta-operator
USER 65532:65532
ENTRYPOINT ["/karta-operator"]
