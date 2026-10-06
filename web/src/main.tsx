import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router-dom";
import { App } from "./App";
import { PwaProvider } from "./components/Pwa";
import { Viewport } from "./components/Viewport";
import "./styles.css";

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <Viewport />
    <BrowserRouter>
      <PwaProvider><App /></PwaProvider>
    </BrowserRouter>
  </StrictMode>,
);
