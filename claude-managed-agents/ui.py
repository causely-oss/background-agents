# Copyright 2026 Anthropic PBC
# SPDX-License-Identifier: Apache-2.0
"""Tiny shared UI helper — load the dark-terminal CSS theme."""
from pathlib import Path
import streamlit as st

_ROOT = Path(__file__).parent


def inject_style():
    css = (_ROOT / "assets" / "style.css").read_text()
    st.markdown(f"<style>{css}</style>", unsafe_allow_html=True)
