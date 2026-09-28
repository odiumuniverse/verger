import { appendFileSync } from "node:fs";

export default function (pi: any) {
  pi.on("tool_call", (event: any) => {
    appendFileSync(process.env.HOME + "/guard.log", JSON.stringify(event.input) + "\n");

    if (event.input?.command?.includes("BLOCKME")) {
      return { block: true, reason: "blocked by the verger fixture" };
    }
  });
}
