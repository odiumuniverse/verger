import { appendFileSync } from "node:fs";

export default function (pi) {
  pi.on("session_start", () => appendFileSync(process.env.HOME + "/audit.log", "started\n"));
}
