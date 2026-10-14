# Third-party notices

Vornik ships the following third-party components in its own tree (vendored,
not fetched at build time). Each keeps its original licence, reproduced
below as that licence requires.

Go module dependencies are not vendored. They are inventoried, with their
licences, in the CycloneDX SBOM attached to every release
(`vornik.cdx.json` on the GitHub release page), which is the authoritative
list for that layer.

## htmx 1.9.10 and the htmx SSE extension

Files: `internal/ui/static/htmx.min.js`, `internal/ui/static/htmx-ext-sse.js`
Source: https://github.com/bigskysoftware/htmx (tag `v1.9.10`)
Licence: BSD 2-Clause

```
Copyright (c) 2020, Big Sky Software
All rights reserved.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are met:

1. Redistributions of source code must retain the above copyright notice, this
   list of conditions and the following disclaimer.

2. Redistributions in binary form must reproduce the above copyright notice,
   this list of conditions and the following disclaimer in the documentation
   and/or other materials provided with the distribution.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE
FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL
DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR
SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER
CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY,
OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
```

## Third-party programs in the agent image

Files: `images/vornik-agent/third-party/` (licence texts, copied unmodified
from the upstream commits named there, plus two notices marked `.vornik`)
Licence: each component's own, with the third-party notices it ships:

- espeak-ng: GPL-3.0-or-later (`espeak-ng/COPYING`); its getopt
  compatibility code is BSD-2-Clause (`COPYING.BSD2`), and the upstream
  repository's other licence files, Apache-2.0 (`COPYING.APACHE`) and the
  Unicode data licence (`COPYING.UCD`), are reproduced unmodified;
  `NOTICE.vornik` lists its copyright holders.
- onnxruntime: MIT (`onnxruntime/LICENSE`), plus the notices of the many
  third-party components it incorporates, under their own licences
  (`ThirdPartyNotices.txt`, unmodified from upstream).
- piper and piper-phonemize: MIT; piper-phonemize compiles in nlohmann/json
  (MIT, `nlohmann-json-LICENSE.vornik`) and uni-algo (public domain or MIT,
  at the user's choice, `uni-algo-LICENSE.md`).
- spdlog: MIT, with the fmt it bundles (MIT, `fmt-9.1.0-LICENSE.rst`).
- libtashkeel and whisper.cpp: MIT.

The agent image ships the rhasspy/piper text-to-speech bundle (piper,
piper-phonemize, onnxruntime, espeak-ng and the libraries above) and
whisper.cpp. Their licence texts and copyright notices are kept in that
directory and installed in the image under `/usr/share/licenses/`;
`SOURCE-OFFER` there names the exact upstream commits and where the
corresponding source is. They are not reproduced here because they are not
part of this source tree's programs.

The piper source archive (`vornik-agent-third-party-source-piper-2023.11.14-2.tar`,
attached to Vornik Community Edition release 2026.10.6 and later) is also
the corresponding source for the agent images distributed before it with the
same bundle: releases 2026.9.7, 2026.9.8, 2026.10.1, 2026.10.2, 2026.10.3,
2026.10.4 and 2026.10.5, and every agent image built from the main branch
since 2026-09-26, when the bundle entered the image. All of them carry piper
release 2023.11.14-2, the only release the image has used.

## Adding a vendored file

If you copy a third-party file into this tree, add a section here with the
file path, the upstream source and version, the licence name, and the
licence text. The SBOM does not see vendored files; this page is the only
record of them.
