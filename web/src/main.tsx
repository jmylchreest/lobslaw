import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router-dom";
import { App } from "./App";
import { PwaProvider } from "./components/Pwa";
import "./styles.css";

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <BrowserRouter>
      <PwaProvider><App /></PwaProvider>
    </BrowserRouter>
  </StrictMode>,
);
