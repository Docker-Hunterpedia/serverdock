# Assets

- `og.png` (1280×640): the README hero and the GitHub social preview. Its source is `og.html`.

To re-render after editing `og.html` (macOS, Chrome installed):

```sh
cd docs/assets
"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" --headless=new --disable-gpu \
  --hide-scrollbars --force-device-scale-factor=1 --window-size=1280,640 \
  --screenshot="$PWD/og.png" "file://$PWD/og.html"
```

The numbers in the image (54 MB, 0.00 %) are the M3a CI bench (2026-11-28). Update them when a release changes them.

GitHub's social preview is set by hand: repository Settings → General → Social preview → upload `og.png`.
