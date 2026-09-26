// Stand-in for a real project: prints a few lines so we can see the container
// actually ran, then exits.
const os = require("os");

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

async function main() {
  console.log(`sample project starting on ${os.hostname()}`);

  for (let i = 1; i <= 5; i++) {
    console.log(`sample project tick ${i}`);
    await sleep(1000);
  }

  console.log("sample project finished");
}

main();
