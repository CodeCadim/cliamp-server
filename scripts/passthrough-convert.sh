#!/usr/bin/env bash
# Prepare a music library for stations with `passthrough = true`.
#
# Mirrors SRC into DST so that every audio file becomes an MP3 in the station
# profile (MPEG-1 Layer III, 128 kbps CBR, 44.1 kHz, stereo). Files already in
# that profile are copied as they are (no generation loss); everything else is
# re-encoded. Tags are kept, cover art is dropped.
#
# Re-running is incremental: a DST file newer than its source is left alone.
#
# usage: passthrough-convert.sh SRC DST
# needs: ffmpeg (with libmp3lame) and ffprobe
set -euo pipefail

if [[ $# -ne 2 ]]; then
	echo "usage: $0 SRC DST" >&2
	exit 2
fi
src=$(realpath "$1")
dst=$(realpath -m "$2")
case "$dst/" in "$src"/*)
	echo "DST must not be inside SRC" >&2
	exit 2
	;;
esac

for tool in ffmpeg ffprobe; do
	command -v "$tool" >/dev/null || { echo "$tool not found" >&2; exit 1; }
done

# in_profile FILE: MP3 at 44.1 kHz whose every packet is a 128 kbps frame
# (417 or 418 bytes). Packet sizes come from the demuxer, nothing is decoded.
in_profile() {
	local info sizes
	info=$(ffprobe -v error -select_streams a:0 \
		-show_entries stream=codec_name,sample_rate,channels -of csv=p=0 "$1") || return 1
	[[ $info == "mp3,44100,2" ]] || return 1
	sizes=$(ffprobe -v error -select_streams a:0 -show_entries packet=size \
		-of default=nw=1:nk=1 "$1" | sort -u | tr '\n' ' ') || return 1
	[[ $sizes == "417 418 " || $sizes == "417 " || $sizes == "418 " ]]
}

copied=0 encoded=0 skipped=0 failed=0
while IFS= read -r -d '' in; do
	rel=${in#"$src"/}
	out="$dst/${rel%.*}.mp3"
	if [[ -f $out && $out -nt $in ]]; then
		skipped=$((skipped + 1))
		continue
	fi
	mkdir -p "$(dirname "$out")"
	tmp="$out.part.mp3"

	if [[ ${in,,} == *.mp3 ]] && in_profile "$in"; then
		cp "$in" "$tmp" && mv "$tmp" "$out" && copied=$((copied + 1)) && echo "copy    $rel"
		continue
	fi

	if ffmpeg -nostdin -v error -y -i "$in" -map 0:a:0 \
		-c:a libmp3lame -b:a 128k -ar 44100 -ac 2 \
		-map_metadata 0 -id3v2_version 3 -write_id3v1 0 "$tmp" && mv "$tmp" "$out"; then
		encoded=$((encoded + 1))
		echo "encode  $rel"
	else
		rm -f "$tmp"
		failed=$((failed + 1))
		echo "FAILED  $rel" >&2
	fi
done < <(find "$src" -type f \( -iname '*.mp3' -o -iname '*.wav' -o -iname '*.flac' \
	-o -iname '*.ogg' -o -iname '*.opus' -o -iname '*.m4a' -o -iname '*.aac' \
	-o -iname '*.webm' -o -iname '*.wma' \) -print0 | sort -z)

echo "done: $copied copied, $encoded encoded, $skipped up to date, $failed failed"
[[ $failed -eq 0 ]]
