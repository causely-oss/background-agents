# Copyright 2026 Anthropic PBC
# SPDX-License-Identifier: Apache-2.0
"""A Claude Managed Agent wired to remote MCP servers (see README.md)."""
import streamlit as st
from dotenv import load_dotenv

load_dotenv()

from ui import inject_style
from provided import chat_panel

st.set_page_config(page_title="Managed Agent · Remote MCP", page_icon="▮", layout="wide")
inject_style()

st.title("MANAGED AGENT")
st.caption(
    "A Claude Managed Agent investigating a Kubernetes cluster through remote MCP servers. "
    "See README.md for setup."
)

chat_panel()
