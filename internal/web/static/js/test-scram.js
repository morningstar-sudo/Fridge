// RFC 7677 section 3 test vector, run in the browser: prints PASS or FAIL to #result.
import { clientProof } from "./scram.js";

const out = document.getElementById("result");
try {
  const nonce = "rOprNGfwEbeRWgbNEkqO";
  const serverFirst = "r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096";
  const { clientFinal, serverFinal } = await clientProof("user", "pencil", nonce, serverFirst);
  const wantFinal = "c=biws,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,p=dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ=";
  const wantServer = "v=6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4=";
  const ok = clientFinal === wantFinal && serverFinal === wantServer;
  out.textContent = (ok ? "PASS" : "FAIL") + "\nclient-final: " + clientFinal + "\nserver-final: " + serverFinal;
} catch (e) {
  out.textContent = "FAIL " + e.message;
}
