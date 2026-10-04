import { useEffect } from "react";

/** iOS resizes the visual viewport for its keyboard, but can leave dvh unchanged. */
export function Viewport() {
  useEffect(() => {
    const viewport = window.visualViewport;
    const root = document.documentElement;
    let frame = 0;
    const update = () => {
      cancelAnimationFrame(frame);
      frame = requestAnimationFrame(() => {
        if (!viewport || viewport.scale > 1.01 || !window.matchMedia("(max-width: 860px)").matches) {
          root.style.removeProperty("--visible-height"); root.style.removeProperty("--visible-top"); root.classList.remove("keyboard-open");
          return;
        }
        root.style.setProperty("--visible-height", `${Math.round(viewport.height)}px`);
        root.style.setProperty("--visible-top", `${Math.round(viewport.offsetTop)}px`);
        root.classList.toggle("keyboard-open", window.innerHeight - viewport.height > 120);
      });
    };
    update();
    viewport?.addEventListener("resize", update); viewport?.addEventListener("scroll", update);
    window.addEventListener("resize", update);
    return () => {
      cancelAnimationFrame(frame);
      viewport?.removeEventListener("resize", update); viewport?.removeEventListener("scroll", update);
      window.removeEventListener("resize", update);
      root.style.removeProperty("--visible-height"); root.style.removeProperty("--visible-top"); root.classList.remove("keyboard-open");
    };
  }, []);
  return null;
}
