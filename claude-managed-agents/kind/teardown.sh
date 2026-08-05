#!/usr/bin/env bash
# Copyright 2026 Anthropic PBC
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail
kind delete cluster --name managed-agent-demo
