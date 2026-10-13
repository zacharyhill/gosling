const vscode = require("vscode");
const { LanguageClient } = require("vscode-languageclient/node");

let client;

async function startClient() {
  const config = vscode.workspace.getConfiguration("gosling");
  const args = ["lsp", "-gopls", config.get("goplsPath", "gopls")];
  if (config.get("generateOnSave", true)) {
    args.push("-generate-on-save");
  }
  const serverOptions = { command: config.get("path", "gosling"), args };
  const clientOptions = { documentSelector: [{ scheme: "file", language: "gol" }] };
  client = new LanguageClient("gosling", "gosling", serverOptions, clientOptions);
  try {
    await client.start();
  } catch (err) {
    vscode.window.showErrorMessage(`The gosling language server did not start: ${err.message}`);
  }
}

async function stopClient() {
  if (!client) {
    return;
  }
  try {
    await client.stop();
  } catch (err) {
    vscode.window.showErrorMessage(`The gosling language server did not stop: ${err.message}`);
  }
  client = undefined;
}

async function restartClient() {
  await stopClient();
  await startClient();
}

async function activate(context) {
  context.subscriptions.push(
    vscode.commands.registerCommand("gosling.restartServer", restartClient),
    // The settings become server flags, so a change needs a new server.
    vscode.workspace.onDidChangeConfiguration(async (event) => {
      if (event.affectsConfiguration("gosling")) {
        await restartClient();
      }
    }),
  );
  await startClient();
}

async function deactivate() {
  await stopClient();
}

module.exports = { activate, deactivate };
