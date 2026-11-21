#!/bin/sh
# Install only the local parser runtime; no service or account is contacted.
set -eu
runtime_dir=${1:-plugins/document-runtime}
python_bin=${PYTHON_BIN:-python3}
"$python_bin" -m venv "$runtime_dir"
"$runtime_dir/bin/python" -m pip install --disable-pip-version-check 'pypdfium2==5.12.1' 'xlrd==2.0.2'
"$runtime_dir/bin/python" -c 'import pypdfium2, xlrd; print("Document parser runtime is ready.")'
