import type { Metadata } from "next";
import { Geist, Geist_Mono } from "next/font/google";
import { ClerkProvider } from "@clerk/nextjs";
import { shadcn } from "@clerk/ui/themes";

import { SiteHeader } from "@/components/site-header";
import "./globals.css";

const geistSans = Geist({
  variable: "--font-geist-sans",
  subsets: ["latin"],
});

const geistMono = Geist_Mono({
  variable: "--font-geist-mono",
  subsets: ["latin"],
});

export const metadata: Metadata = {
  title: "Weave",
  description: "The shared control room for human and AI work.",
};

/** Applies the shared fonts and global document structure to every route. */
export default function RootLayout({ children }: LayoutProps<"/">) {
  return (
    // ClerkProvider sits inside <body>, not around <html>. Wrapping the
    // document element makes the provider own <html>'s render, which
    // interferes with the attributes Next.js and hydration set on it.
    <html lang="en" className={`${geistSans.variable} ${geistMono.variable} h-full antialiased`}>
      <body className="min-h-full flex flex-col">
        <ClerkProvider appearance={{ theme: shadcn }}>
          <SiteHeader />
          {children}
        </ClerkProvider>
      </body>
    </html>
  );
}
