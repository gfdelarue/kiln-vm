export const curlRetryArgs = [
  "--connect-timeout",
  "20",
  "--retry",
  "6",
  "--retry-delay",
  "10",
  "--retry-max-time",
  "300",
  "--retry-all-errors",
];

export function curlDownloadArgs(output, url) {
  return [
    "--fail",
    "--location",
    "--show-error",
    "--silent",
    ...curlRetryArgs,
    "--output",
    output,
    url,
  ];
}

export function curlTextArgs(url) {
  return [
    "--fail",
    "--location",
    "--show-error",
    "--silent",
    ...curlRetryArgs,
    url,
  ];
}
